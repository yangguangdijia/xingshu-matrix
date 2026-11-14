package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/backup"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/config"
)

// restoreMain implements `matrix-server restore --file X --yes [-config path]`.
// It runs pg_restore --clean --if-exists against database.dsn; the password is
// passed only through PGPASSWORD. Returns the process exit code.
func restoreMain(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	cfgPath := fs.String("config", os.Getenv("MATRIX_CONFIG"), "配置文件路径（环境变量 MATRIX_CONFIG）")
	file := fs.String("file", "", "要恢复的备份文件（pg_dump --format=custom 生成的 .dump）")
	yes := fs.Bool("yes", false, "确认覆盖当前数据库（必填）")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "用法: matrix-server restore --file /var/lib/matrix/backups/xs-20261114T020000Z.dump --yes [-config config.yaml]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "错误：必须通过 --file 指定备份文件")
		fs.Usage()
		return 2
	}
	if err := checkDumpFile(*file); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "警告：恢复会先删除（--clean）当前数据库中备份所含的全部对象再重建，现有数据将被覆盖且不可撤销。")
		fmt.Fprintln(os.Stderr, "请先停止所有 matrix-server 实例，确认无误后加上 --yes 重新执行。")
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：加载配置失败：", err)
		return 1
	}
	bin, err := backup.LookTool(cfg.Backup.PgRestore, "pg_restore", "backup.pg_restore / MATRIX_PG_RESTORE")
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	conn, password, err := backup.ConnArgs(cfg.Database.DSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		return 1
	}
	sum, err := fileSHA256(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：读取备份文件失败：", err)
		return 1
	}

	fmt.Printf("即将恢复备份：%s\nSHA-256：%s\n（请与平台备份列表中的校验值核对）\n", *file, sum)
	fmt.Println("请确认所有 matrix-server 实例已停止，恢复过程中请勿中断……")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(ctx, bin, "--clean", "--if-exists", "--no-owner", "--no-password", "--dbname="+conn, *file)
	cmd.Env = backup.PGEnv(password)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "恢复未完全成功：pg_restore 退出码 %d，请检查上方输出（部分对象可能已被重建）。\n", ee.ExitCode())
		} else {
			fmt.Fprintln(os.Stderr, "恢复失败：", err)
		}
		return 1
	}
	fmt.Println("恢复完成。启动 matrix-server 时将自动执行尚未应用的数据库迁移；请登录平台核对数据。")
	return 0
}

// checkDumpFile verifies the file exists and carries the pg_dump custom-format magic "PGDMP".
func checkDumpFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("无法打开备份文件 %s：%w", path, err)
	}
	defer f.Close()
	magic := make([]byte, 5)
	if _, err := io.ReadFull(bufio.NewReader(f), magic); err != nil || string(magic) != "PGDMP" {
		return fmt.Errorf("%s 不是 pg_dump 自定义格式（--format=custom）的备份文件", path)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
