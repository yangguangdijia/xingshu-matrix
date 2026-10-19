package ai

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/crypto"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/store"
)

// ModelConfig is an OpenAI-compatible endpoint configuration.
type ModelConfig struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Provider     string            `json:"provider"`
	BaseURL      string            `json:"base_url"`
	APIKeyEnc    string            `json:"-"`
	APIKeyMasked string            `json:"api_key_masked"`
	HasAPIKey    bool              `json:"has_api_key"`
	Model        string            `json:"model"`
	Temperature  float32           `json:"temperature"`
	MaxTokens    int               `json:"max_tokens"`
	MaxContext   int               `json:"max_context"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	IsDefault    bool              `json:"is_default"`
	Enabled      bool              `json:"enabled"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// ModelInput is the create/update payload. APIKey: nil keeps the stored key on
// update, "" clears it.
type ModelInput struct {
	Name         string            `json:"name" binding:"required,max=64"`
	Provider     string            `json:"provider" binding:"max=64"`
	BaseURL      string            `json:"base_url" binding:"required,max=512"`
	APIKey       *string           `json:"api_key"`
	Model        string            `json:"model" binding:"required,max=128"`
	Temperature  *float32          `json:"temperature"`
	MaxTokens    int               `json:"max_tokens" binding:"min=0,max=1000000"`
	MaxContext   int               `json:"max_context" binding:"min=0,max=10000000"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	IsDefault    bool              `json:"is_default"`
	Enabled      *bool             `json:"enabled"`
}

// PromptTemplate is a scene prompt (system prompt) template rendered with Go
// text/template syntax, e.g. "你是 {{.cluster}} 集群的 K8s 诊断专家".
type PromptTemplate struct {
	ID          int64     `json:"id"`
	Scene       string    `json:"scene"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Content     string    `json:"content"`
	IsDefault   bool      `json:"is_default"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PromptInput is the create/update payload for templates.
type PromptInput struct {
	Scene       string `json:"scene" binding:"required,max=64"`
	Name        string `json:"name" binding:"required,max=128"`
	Description string `json:"description" binding:"max=512"`
	Content     string `json:"content" binding:"required,max=65536"`
	IsDefault   bool   `json:"is_default"`
	Enabled     *bool  `json:"enabled"`
}

// Render executes the template with vars (missing keys render as empty).
func (t *PromptTemplate) Render(vars map[string]any) (string, error) {
	tpl, err := template.New(t.Name).Option("missingkey=zero").Parse(t.Content)
	if err != nil {
		return "", httpx.Validation("invalid prompt template: " + err.Error())
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, vars); err != nil {
		return "", httpx.Validation("render prompt template: " + err.Error())
	}
	return strings.ReplaceAll(sb.String(), "<no value>", ""), nil
}

// Store persists models, templates and pending tool calls.
type Store struct {
	DB     store.DB
	Cipher *crypto.Cipher
}

const apiKeyAAD = "ai_models.api_key"

func (s *Store) decorate(m *ModelConfig) {
	if m.APIKeyEnc != "" {
		m.HasAPIKey = true
		if k, err := s.Cipher.DecryptBytes(m.APIKeyEnc, []byte(apiKeyAAD)); err == nil {
			m.APIKeyMasked = crypto.Mask(string(k))
		} else {
			m.APIKeyMasked = "(undecryptable: encryption key changed?)"
		}
	}
	if m.ExtraHeaders == nil {
		m.ExtraHeaders = map[string]string{}
	}
}

// APIKey decrypts the model's API key.
func (s *Store) APIKey(m *ModelConfig) (string, error) {
	if m.APIKeyEnc == "" {
		return "", nil
	}
	b, err := s.Cipher.DecryptBytes(m.APIKeyEnc, []byte(apiKeyAAD))
	return string(b), err
}

const modelCols = `id, name, provider, base_url, api_key_enc, model, temperature, max_tokens, max_context, extra_headers, is_default, enabled, created_at, updated_at`

func (s *Store) scanModel(row pgx.Row) (*ModelConfig, error) {
	var m ModelConfig
	var hdr []byte
	err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.APIKeyEnc, &m.Model, &m.Temperature, &m.MaxTokens, &m.MaxContext, &hdr,
		&m.IsDefault, &m.Enabled, &m.CreatedAt, &m.UpdatedAt)
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("model not found")
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(hdr, &m.ExtraHeaders)
	s.decorate(&m)
	return &m, nil
}

// ListModels returns all model configs.
func (s *Store) ListModels(ctx context.Context) ([]ModelConfig, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+modelCols+` FROM ai_models ORDER BY is_default DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelConfig
	for rows.Next() {
		m, err := s.scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	if out == nil {
		out = []ModelConfig{}
	}
	return out, rows.Err()
}

// GetModel returns one model.
func (s *Store) GetModel(ctx context.Context, id int64) (*ModelConfig, error) {
	return s.scanModel(s.DB.QueryRow(ctx, `SELECT `+modelCols+` FROM ai_models WHERE id=$1`, id))
}

// ResolveModel returns the enabled model id, or the default one when id == 0.
func (s *Store) ResolveModel(ctx context.Context, id int64) (*ModelConfig, error) {
	var m *ModelConfig
	var err error
	if id > 0 {
		m, err = s.GetModel(ctx, id)
	} else {
		m, err = s.scanModel(s.DB.QueryRow(ctx, `SELECT `+modelCols+` FROM ai_models WHERE enabled ORDER BY is_default DESC, id LIMIT 1`))
		if err != nil {
			if e, ok := err.(*httpx.Error); ok && e.Code == httpx.CodeNotFound {
				return nil, httpx.Validation("尚未配置可用的 AI 模型，请先在「AI 设置」中添加模型")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if !m.Enabled {
		return nil, httpx.Validation("model is disabled")
	}
	return m, nil
}

func validateModel(in *ModelInput) error {
	u, err := url.Parse(strings.TrimSpace(in.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return httpx.Validation("base_url must be an http(s) URL")
	}
	if in.Provider == "" {
		in.Provider = "openai-compatible"
	}
	for k := range in.ExtraHeaders {
		if strings.EqualFold(k, "authorization") || strings.ContainsAny(k, " \r\n:") {
			return httpx.Validation("invalid extra header: " + k)
		}
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		return httpx.Validation("temperature must be within [0,2]")
	}
	return nil
}

// CreateModel inserts a model (the first model becomes default).
func (s *Store) CreateModel(ctx context.Context, in ModelInput) (*ModelConfig, error) {
	if err := validateModel(&in); err != nil {
		return nil, err
	}
	enc := ""
	if in.APIKey != nil && *in.APIKey != "" {
		var err error
		if enc, err = s.Cipher.EncryptBytes([]byte(*in.APIKey), []byte(apiKeyAAD)); err != nil {
			return nil, err
		}
	}
	temp := float32(0.2)
	if in.Temperature != nil {
		temp = *in.Temperature
	}
	enabled := in.Enabled == nil || *in.Enabled
	hdr, _ := json.Marshal(nonNilMap(in.ExtraHeaders))
	var id int64
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ai_models`).Scan(&n); err != nil {
			return err
		}
		isDefault := in.IsDefault || n == 0
		if isDefault {
			if _, err := tx.Exec(ctx, `UPDATE ai_models SET is_default=FALSE WHERE is_default`); err != nil {
				return err
			}
		}
		return tx.QueryRow(ctx, `INSERT INTO ai_models(name, provider, base_url, api_key_enc, model, temperature, max_tokens, max_context, extra_headers, is_default, enabled)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11) RETURNING id`,
			in.Name, in.Provider, strings.TrimSpace(in.BaseURL), enc, in.Model, temp, in.MaxTokens, in.MaxContext, string(hdr), isDefault, enabled).Scan(&id)
	})
	if store.IsUniqueViolation(err) {
		return nil, httpx.Conflict("model name already exists")
	}
	if err != nil {
		return nil, err
	}
	return s.GetModel(ctx, id)
}

// UpdateModel updates a model.
func (s *Store) UpdateModel(ctx context.Context, id int64, in ModelInput) (*ModelConfig, error) {
	cur, err := s.GetModel(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := validateModel(&in); err != nil {
		return nil, err
	}
	enc := cur.APIKeyEnc
	if in.APIKey != nil {
		enc = ""
		if *in.APIKey != "" {
			if enc, err = s.Cipher.EncryptBytes([]byte(*in.APIKey), []byte(apiKeyAAD)); err != nil {
				return nil, err
			}
		}
	}
	temp := cur.Temperature
	if in.Temperature != nil {
		temp = *in.Temperature
	}
	enabled := cur.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	hdr, _ := json.Marshal(nonNilMap(in.ExtraHeaders))
	err = store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if in.IsDefault {
			if _, err := tx.Exec(ctx, `UPDATE ai_models SET is_default=FALSE WHERE is_default AND id<>$1`, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE ai_models SET name=$2, provider=$3, base_url=$4, api_key_enc=$5, model=$6, temperature=$7,
			max_tokens=$8, max_context=$9, extra_headers=$10::jsonb, is_default=$11, enabled=$12, updated_at=now() WHERE id=$1`,
			id, in.Name, in.Provider, strings.TrimSpace(in.BaseURL), enc, in.Model, temp, in.MaxTokens, in.MaxContext, string(hdr),
			in.IsDefault || (cur.IsDefault && enabled), enabled)
		return err
	})
	if store.IsUniqueViolation(err) {
		return nil, httpx.Conflict("model name already exists")
	}
	if err != nil {
		return nil, err
	}
	return s.GetModel(ctx, id)
}

// DeleteModel removes a model.
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM ai_models WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.NotFound("model not found")
	}
	return err
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

const promptCols = `id, scene, name, description, content, is_default, enabled, created_at, updated_at`

func scanPrompt(row pgx.Row) (*PromptTemplate, error) {
	var t PromptTemplate
	err := row.Scan(&t.ID, &t.Scene, &t.Name, &t.Description, &t.Content, &t.IsDefault, &t.Enabled, &t.CreatedAt, &t.UpdatedAt)
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("prompt template not found")
	}
	return &t, err
}

// ListPrompts lists templates, optionally filtered by scene.
func (s *Store) ListPrompts(ctx context.Context, scene string) ([]PromptTemplate, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+promptCols+` FROM ai_prompt_templates WHERE ($1='' OR scene=$1) ORDER BY scene, is_default DESC, name`, scene)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptTemplate{}
	for rows.Next() {
		t, err := scanPrompt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetPrompt returns one template.
func (s *Store) GetPrompt(ctx context.Context, id int64) (*PromptTemplate, error) {
	return scanPrompt(s.DB.QueryRow(ctx, `SELECT `+promptCols+` FROM ai_prompt_templates WHERE id=$1`, id))
}

// ScenePrompt returns the enabled default (or first enabled) template of scene.
func (s *Store) ScenePrompt(ctx context.Context, scene string) (*PromptTemplate, error) {
	t, err := scanPrompt(s.DB.QueryRow(ctx, `SELECT `+promptCols+` FROM ai_prompt_templates
		WHERE scene=$1 AND enabled ORDER BY is_default DESC, id LIMIT 1`, scene))
	if err != nil {
		if e, ok := err.(*httpx.Error); ok && e.Code == httpx.CodeNotFound {
			return nil, nil
		}
	}
	return t, err
}

func (s *Store) savePrompt(ctx context.Context, id int64, in PromptInput) (int64, error) {
	if _, err := (&PromptTemplate{Name: in.Name, Content: in.Content}).Render(nil); err != nil {
		return 0, err
	}
	enabled := in.Enabled == nil || *in.Enabled
	err := store.InTx(ctx, s.DB, func(tx pgx.Tx) error {
		if in.IsDefault {
			if _, err := tx.Exec(ctx, `UPDATE ai_prompt_templates SET is_default=FALSE WHERE scene=$1 AND is_default AND id<>$2`, in.Scene, id); err != nil {
				return err
			}
		}
		if id == 0 {
			return tx.QueryRow(ctx, `INSERT INTO ai_prompt_templates(scene, name, description, content, is_default, enabled)
				VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, in.Scene, in.Name, in.Description, in.Content, in.IsDefault, enabled).Scan(&id)
		}
		tag, err := tx.Exec(ctx, `UPDATE ai_prompt_templates SET scene=$2, name=$3, description=$4, content=$5, is_default=$6,
			enabled=$7, updated_at=now() WHERE id=$1`, id, in.Scene, in.Name, in.Description, in.Content, in.IsDefault, enabled)
		if err == nil && tag.RowsAffected() == 0 {
			return httpx.NotFound("prompt template not found")
		}
		return err
	})
	if store.IsUniqueViolation(err) {
		return 0, httpx.Conflict("a template with this scene and name already exists")
	}
	return id, err
}

// CreatePrompt inserts a template.
func (s *Store) CreatePrompt(ctx context.Context, in PromptInput) (*PromptTemplate, error) {
	id, err := s.savePrompt(ctx, 0, in)
	if err != nil {
		return nil, err
	}
	return s.GetPrompt(ctx, id)
}

// UpdatePrompt updates a template.
func (s *Store) UpdatePrompt(ctx context.Context, id int64, in PromptInput) (*PromptTemplate, error) {
	if _, err := s.savePrompt(ctx, id, in); err != nil {
		return nil, err
	}
	return s.GetPrompt(ctx, id)
}

// DeletePrompt removes a template.
func (s *Store) DeletePrompt(ctx context.Context, id int64) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM ai_prompt_templates WHERE id=$1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return httpx.NotFound("prompt template not found")
	}
	return err
}

// PendingCall is a tool call awaiting user confirmation.
type PendingCall struct {
	ID         string          `json:"id"`
	UserID     int64           `json:"user_id"`
	Tool       string          `json:"tool"`
	ToolCallID string          `json:"tool_call_id"`
	Arguments  json.RawMessage `json:"arguments"`
	Status     string          `json:"status"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// InsertPendingCall stores a pending confirmation.
func (s *Store) InsertPendingCall(ctx context.Context, c *PendingCall) error {
	return s.DB.QueryRow(ctx, `INSERT INTO ai_tool_calls(id, user_id, tool, tool_call_id, arguments) VALUES ($1,$2,$3,$4,$5::jsonb) RETURNING created_at, status`,
		c.ID, c.UserID, c.Tool, c.ToolCallID, string(c.Arguments)).Scan(&c.CreatedAt, &c.Status)
}

// ClaimPendingCall atomically moves a pending call of userID to "claimed" status
// target (executed/rejected/failed placeholder) and returns it.
func (s *Store) ClaimPendingCall(ctx context.Context, id string, userID int64, status string, maxAge time.Duration) (*PendingCall, error) {
	var c PendingCall
	var args []byte
	err := s.DB.QueryRow(ctx, `UPDATE ai_tool_calls SET status=$3, decided_at=now()
		WHERE id=$1 AND user_id=$2 AND status='pending' AND created_at > now() - make_interval(secs => $4)
		RETURNING id, user_id, tool, tool_call_id, arguments, status, created_at`, id, userID, status, maxAge.Seconds()).
		Scan(&c.ID, &c.UserID, &c.Tool, &c.ToolCallID, &args, &c.Status, &c.CreatedAt)
	if store.IsNoRows(err) {
		return nil, httpx.NotFound("tool call not found, already decided or expired")
	}
	c.Arguments = args
	return &c, err
}

// FinishCall records the outcome of a confirmed call.
func (s *Store) FinishCall(ctx context.Context, id, status string, result json.RawMessage, errMsg string) error {
	var res any
	if len(result) > 0 {
		res = string(result)
	}
	_, err := s.DB.Exec(ctx, `UPDATE ai_tool_calls SET status=$2, result=$3::jsonb, error=$4 WHERE id=$1`, id, status, res, errMsg)
	return err
}
