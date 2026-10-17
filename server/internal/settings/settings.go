// Package settings stores platform settings as JSON values keyed by dotted
// names ("notify.dingtalk.webhook"). Secret settings are AES-GCM encrypted and
// masked on read. Keys prefixed "public." are readable by every signed-in user.
package settings

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/audit"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// Setting is one stored value.
type Setting struct {
	Key         string          `json:"key"`
	Value       json.RawMessage `json:"value"`
	Secret      bool            `json:"secret"`
	HasValue    bool            `json:"has_value"`
	Description string          `json:"description"`
	UpdatedBy   *int64          `json:"updated_by"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Service reads and writes settings.
type Service struct {
	DB     store.DB
	Cipher *crypto.Cipher
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_\-]*(\.[a-z0-9_\-]+)*$`)

const aadPrefix = "settings:"

// ValidKey reports whether k is a valid setting key.
func ValidKey(k string) bool { return len(k) <= 128 && keyRe.MatchString(k) }

func scan(row pgx.Row) (*Setting, error) {
	var s Setting
	var v []byte
	err := row.Scan(&s.Key, &v, &s.Secret, &s.Description, &s.UpdatedBy, &s.UpdatedAt)
	if store.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	s.Value = v
	return &s, err
}

func mask(s *Setting) {
	if s.Secret {
		s.HasValue = len(s.Value) > 0 && string(s.Value) != `""`
		s.Value = json.RawMessage(`"******"`)
		return
	}
	s.HasValue = true
}

// List returns settings (secrets masked), optionally by key prefix.
func (s *Service) List(ctx context.Context, prefix string) ([]Setting, error) {
	rows, err := s.DB.Query(ctx, `SELECT key, value, secret, description, updated_by, updated_at FROM settings
		WHERE key LIKE $1::text || '%' ORDER BY key`, strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Setting{}
	for rows.Next() {
		st, err := scan(rows)
		if err != nil {
			return nil, err
		}
		mask(st)
		out = append(out, *st)
	}
	return out, rows.Err()
}

// GetMasked returns one setting with secrets masked.
func (s *Service) GetMasked(ctx context.Context, key string) (*Setting, error) {
	st, err := scan(s.DB.QueryRow(ctx, `SELECT key, value, secret, description, updated_by, updated_at FROM settings WHERE key=$1`, key))
	if err != nil {
		return nil, err
	}
	mask(st)
	return st, nil
}

// Get decodes a setting into dst (decrypting secrets). Returns httpx.ErrNotFound when absent.
func (s *Service) Get(ctx context.Context, key string, dst any) error {
	st, err := scan(s.DB.QueryRow(ctx, `SELECT key, value, secret, description, updated_by, updated_at FROM settings WHERE key=$1`, key))
	if err != nil {
		return err
	}
	raw := []byte(st.Value)
	if st.Secret {
		var env string
		if err := json.Unmarshal(st.Value, &env); err != nil {
			return err
		}
		if raw, err = s.Cipher.DecryptBytes(env, []byte(aadPrefix+key)); err != nil {
			return err
		}
	}
	return json.Unmarshal(raw, dst)
}

// Put stores a value. Secret values are encrypted (the whole JSON value).
func (s *Service) Put(ctx context.Context, key string, value json.RawMessage, secret bool, description string, by int64) (*Setting, error) {
	if !ValidKey(key) {
		return nil, httpx.Validation("invalid setting key (lowercase dotted name)")
	}
	if len(value) == 0 || !json.Valid(value) {
		return nil, httpx.Validation("value must be valid JSON")
	}
	if len(value) > 256*1024 {
		return nil, httpx.Validation("value too large (max 256KiB)")
	}
	stored := []byte(value)
	if secret {
		env, err := s.Cipher.EncryptBytes(value, []byte(aadPrefix+key))
		if err != nil {
			return nil, err
		}
		stored, _ = json.Marshal(env)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO settings(key, value, secret, description, updated_by) VALUES ($1,$2::jsonb,$3,$4,$5)
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, secret=EXCLUDED.secret,
		  description=CASE WHEN EXCLUDED.description='' THEN settings.description ELSE EXCLUDED.description END,
		  updated_by=EXCLUDED.updated_by, updated_at=now()`, key, string(stored), secret, description, by)
	if err != nil {
		return nil, err
	}
	return s.GetMasked(ctx, key)
}

// Delete removes a setting.
func (s *Service) Delete(ctx context.Context, key string) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM settings WHERE key=$1`, key)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.ErrNotFound
	}
	return err
}

// RegisterRoutes mounts /settings endpoints.
func (s *Service) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/settings/public", func(c *gin.Context) {
		items, err := s.List(c.Request.Context(), "public.")
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		out := map[string]json.RawMessage{}
		for _, it := range items {
			if !it.Secret {
				out[it.Key] = it.Value
			}
		}
		httpx.OK(c, out)
	})
	r.GET("/settings", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		items, err := s.List(c.Request.Context(), c.Query("prefix"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, items)
	})
	r.GET("/settings/:key", rbac.RequirePerm("setting:read"), func(c *gin.Context) {
		st, err := s.GetMasked(c.Request.Context(), c.Param("key"))
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, st)
	})
	r.PUT("/settings/:key", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		var in struct {
			Value       json.RawMessage `json:"value" binding:"required"`
			Secret      bool            `json:"secret"`
			Description string          `json:"description" binding:"max=512"`
		}
		if !httpx.Bind(c, &in) {
			return
		}
		audit.SetResource(c, "settings", c.Param("key"))
		if !in.Secret {
			audit.SetDetail(c, gin.H{"value": in.Value})
		}
		st, err := s.Put(c.Request.Context(), c.Param("key"), in.Value, in.Secret, in.Description, rbac.PrincipalFrom(c).UserID)
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, st)
	})
	r.DELETE("/settings/:key", rbac.RequirePerm("setting:manage"), func(c *gin.Context) {
		audit.SetResource(c, "settings", c.Param("key"))
		if err := s.Delete(c.Request.Context(), c.Param("key")); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.OK(c, gin.H{"key": c.Param("key")})
	})
}
