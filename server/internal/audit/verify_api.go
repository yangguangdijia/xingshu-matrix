package audit

import (
	"context"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
)

// Verify limits for GET /audit/chain/verify.
const (
	verifyDefaultLimit = 10000
	verifyMaxLimit     = 50000
	verifyChunk        = 1000
)

// ChainVerifyResult is the response of GET /audit/chain/verify.
type ChainVerifyResult struct {
	Checked  int    `json:"checked"`
	OK       bool   `json:"ok"`
	BreakAt  int64  `json:"break_at"`
	Kind     string `json:"kind"`
	HeadID   int64  `json:"head_id"`
	HeadHash string `json:"head_hash"`
	LastID   int64  `json:"last_id"` // last verified id; pass last_id+1 as from_id to continue
}

// chainVerifier verifies rows fed in ascending-id chunks, carrying the hash
// across chunk boundaries so a stream verifies the same as one VerifyRows call.
type chainVerifier struct {
	prev    string
	checked int
	lastID  int64
	breakAt int64
	kind    string
}

// feed verifies one chunk; it returns false once a break has been found.
func (v *chainVerifier) feed(rows []ChainRow) bool {
	if v.kind != "" {
		return false
	}
	n, at, kind := VerifyRows(v.prev, rows)
	v.checked += n
	if n > 0 {
		v.lastID, v.prev = rows[n-1].ID, rows[n-1].Hash
	}
	if kind != "" {
		v.breakAt, v.kind = at, kind
		return false
	}
	return true
}

const chainRowCols = entryCols + `, prev_hash, hash`

// VerifyLive streams the live segment in id order from max(fromID, start_id)
// up to the segment head (rows committed after the head snapshot are skipped)
// and verifies at most limit rows. Legacy rows below start_id are not checked.
func (s *PgStore) VerifyLive(ctx context.Context, fromID int64, limit int) (*ChainVerifyResult, error) {
	var startID, headID *int64
	var anchor, head string
	if err := s.DB.QueryRow(ctx, `SELECT start_id, anchor_hash, head_id, head_hash FROM audit_chain_segments WHERE kind='live'`).
		Scan(&startID, &anchor, &headID, &head); err != nil {
		return nil, err
	}
	res := &ChainVerifyResult{OK: true, HeadHash: head}
	if headID != nil {
		res.HeadID = *headID
	}
	if startID == nil || headID == nil { // nothing chained yet
		return res, nil
	}
	if anchor == "" {
		anchor = GenesisHash
	}
	from := *startID
	if fromID > from {
		from = fromID
	}
	if from > *headID {
		return res, nil
	}
	v := &chainVerifier{prev: anchor}
	if from > *startID { // resume: anchor is the stored hash of the preceding row
		err := s.DB.QueryRow(ctx, `SELECT hash FROM audit_logs WHERE id < $1 AND id >= $2 ORDER BY id DESC LIMIT 1`, from, *startID).Scan(&v.prev)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT `+chainRowCols+` FROM audit_logs WHERE id >= $1 AND id <= $2 ORDER BY id LIMIT $3`,
		from, *headID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buf := make([]ChainRow, 0, verifyChunk)
	for rows.Next() {
		var r ChainRow
		var detail []byte
		if err := rows.Scan(&r.ID, &r.UserID, &r.Username, &r.IP, &r.Method, &r.Path, &r.Resource, &r.ResourceID, &r.Action,
			&r.StatusCode, &r.Result, &r.DurationMs, &r.RequestID, &r.UserAgent, &detail, &r.CreatedAt, &r.PrevHash, &r.Hash); err != nil {
			return nil, err
		}
		r.Detail = detail
		buf = append(buf, r)
		if len(buf) == verifyChunk {
			if !v.feed(buf) {
				break
			}
			buf = buf[:0]
		}
	}
	if v.kind == "" {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		v.feed(buf)
	}
	res.Checked, res.LastID, res.BreakAt, res.Kind, res.OK = v.checked, v.lastID, v.breakAt, v.kind, v.kind == ""
	return res, nil
}

func parseVerifyQuery(c *gin.Context) (int64, int, error) {
	var fromID int64
	limit := verifyDefaultLimit
	if v := c.Query("from_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, httpx.Validation("invalid from_id")
		}
		fromID = n
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > verifyMaxLimit {
			return 0, 0, httpx.Validation("limit must be 1-" + strconv.Itoa(verifyMaxLimit))
		}
		limit = n
	}
	return fromID, limit, nil
}

func (s *PgStore) handleChainVerify(c *gin.Context) {
	fromID, limit, err := parseVerifyQuery(c)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := s.VerifyLive(c.Request.Context(), fromID, limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}
