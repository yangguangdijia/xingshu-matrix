package bastion

import (
	"regexp"
	"strings"
)

// SQL 风险分级（数据库审查）。
const (
	SQLRead      = "read"      // 只读：可直接在控制台执行（只读事务）
	SQLWrite     = "write"     // 变更：需提交工单审批
	SQLHigh      = "high"      // 高危变更：需审批，界面醒目提示
	SQLForbidden = "forbidden" // 禁止：直接驳回
)

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlStrings      = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.)*"|` + "`[^`]*`")
	sqlWriteWords   = regexp.MustCompile(`\b(insert|update|delete|merge|upsert|replace|create|alter|drop|truncate|rename|grant|revoke|copy|call|exec|execute|lock|vacuum|reindex|cluster|load|handler)\b`)
	sqlForbidden    = regexp.MustCompile(`\b(drop\s+(database|schema|user|role)|shutdown|create\s+(user|role)|alter\s+(user|role|system)|grant|revoke|into\s+outfile|into\s+dumpfile|load_file|pg_read_file|pg_read_binary_file|pg_write_file|lo_import|lo_export|program)\b`)
	sqlNoWhere      = regexp.MustCompile(`^\s*(delete\s+from|update)\b`)
)

// NormalizeSQL 去掉注释与字符串字面量（只用于判断，不用于执行），转小写并压缩空白。
func NormalizeSQL(q string) string {
	q = sqlBlockComment.ReplaceAllString(q, " ")
	q = sqlLineComment.ReplaceAllString(q, " ")
	q = sqlStrings.ReplaceAllString(q, "''")
	return strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

// ClassifySQL 判断一条 SQL 的风险级别并给出原因。只接受单条语句。
// 分级只是第一道关：只读查询在数据库只读事务中执行，变更必须经审批。
func ClassifySQL(q string) (level, reason string) {
	n := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(NormalizeSQL(q)), "; "))
	if n == "" {
		return SQLForbidden, "SQL 为空"
	}
	if strings.Contains(n, ";") {
		return SQLForbidden, "一次只能提交一条语句"
	}
	if sqlForbidden.MatchString(n) {
		return SQLForbidden, "包含被禁止的操作（删库、账号权限、读写服务器文件等）"
	}
	first := strings.SplitN(n, " ", 2)[0]
	switch first {
	case "select", "show", "describe", "desc", "explain", "with", "values", "table":
		if first == "explain" && strings.Contains(n, "analyze") {
			return SQLWrite, "EXPLAIN ANALYZE 会真实执行语句"
		}
		if strings.Contains(n, " for update") || strings.Contains(n, " for share") || (first == "with" && sqlWriteWords.MatchString(n)) {
			return SQLWrite, "包含写操作或行锁"
		}
		return SQLRead, "只读查询"
	}
	switch {
	case first == "drop" || first == "truncate":
		return SQLHigh, "删除表或清空数据，不可恢复"
	case sqlNoWhere.MatchString(n) && !strings.Contains(n, " where "):
		return SQLHigh, "UPDATE / DELETE 没有 WHERE 条件，会影响全表"
	case first == "alter" || first == "rename":
		return SQLHigh, "修改表结构，可能锁表"
	}
	return SQLWrite, "数据变更"
}
