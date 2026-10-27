package cicd

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

func TestHistoricalHelmMigrationChecksum(t *testing.T) {
	migrations, err := store.ParseMigrations(New().Migrations())
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.Version == 4 && migration.Name == "helm" {
			const appliedChecksum = "a33fbf592cb814c78caa6835cbd927147e8df10a31300932f6c8e682f3c92370"
			historicalSQL := []byte(migration.SQL + "\n")
			sum := sha256.Sum256(historicalSQL)
			if got := hex.EncodeToString(sum[:]); got != appliedChecksum {
				t.Fatalf("0004_helm historical checksum = %s, want %s", got, appliedChecksum)
			}
			return
		}
	}
	t.Fatal("0004_helm migration is missing")
}
