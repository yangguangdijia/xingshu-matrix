package httpx

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 200
)

// Page is a parsed ?page=&page_size= pair (page is 1-based).
type Page struct {
	Page int `json:"page"`
	Size int `json:"page_size"`
}

// Offset returns the SQL OFFSET for the page.
func (p Page) Offset() int { return (p.Page - 1) * p.Size }

// Limit returns the SQL LIMIT for the page.
func (p Page) Limit() int { return p.Size }

// ParsePage reads page / page_size (alias size) query params with sane bounds.
func ParsePage(c *gin.Context) Page {
	p := Page{Page: 1, Size: DefaultPageSize}
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		p.Page = v
	}
	sz := c.Query("page_size")
	if sz == "" {
		sz = c.Query("size")
	}
	if v, err := strconv.Atoi(sz); err == nil && v > 0 {
		p.Size = v
	}
	if p.Size > MaxPageSize {
		p.Size = MaxPageSize
	}
	// guard against absurd offsets
	if p.Page > 1_000_000 {
		p.Page = 1_000_000
	}
	return p
}

// PageResult is the standard paginated payload.
type PageResult[T any] struct {
	Items    []T   `json:"items"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// NewPageResult builds a PageResult, normalising nil slices to [].
func NewPageResult[T any](items []T, total int64, p Page) PageResult[T] {
	if items == nil {
		items = []T{}
	}
	return PageResult[T]{Items: items, Total: total, Page: p.Page, PageSize: p.Size}
}

// QueryTime parses an RFC3339 (or unix seconds) query param; zero time if absent.
func QueryTime(c *gin.Context, key string) (time.Time, error) {
	v := strings.TrimSpace(c.Query(key))
	if v == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, Validation("invalid time for " + key + " (RFC3339 or unix seconds)")
	}
	return t, nil
}
