package cluster

import (
	"context"
	"errors"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/httpx"
	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

var resourceCRDGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

type resourceCRDVersion struct {
	Name            string         `json:"name"`
	Served          bool           `json:"served"`
	Storage         bool           `json:"storage"`
	Deprecated      bool           `json:"deprecated"`
	Schema          map[string]any `json:"schema,omitempty"`
	SchemaTruncated bool           `json:"schema_truncated"`
}

type resourceCRDRow struct {
	Name               string               `json:"name"`
	UID                string               `json:"uid"`
	ResourceVersion    string               `json:"resource_version"`
	CreatedAt          string               `json:"created_at"`
	Group              string               `json:"group"`
	Scope              string               `json:"scope"`
	Kind               string               `json:"kind"`
	ListKind           string               `json:"list_kind"`
	Plural             string               `json:"plural"`
	Singular           string               `json:"singular"`
	ShortNames         []string             `json:"short_names"`
	Categories         []string             `json:"categories"`
	Versions           []resourceCRDVersion `json:"versions"`
	StoredVersions     []string             `json:"stored_versions"`
	Conditions         []map[string]string  `json:"conditions"`
	ConversionStrategy string               `json:"conversion_strategy"`
	SchemaProjection   string               `json:"schema_projection,omitempty"`
}

func (m *Module) registerCRDRoutes(g *gin.RouterGroup) {
	read := rbac.RequireAnyPerm(PermClusterRead, PermWorkloadRead)
	g.GET("/:id/crds", read, m.listResourceCRDs)
	g.GET("/:id/crds/:name", read, m.getResourceCRD)
}

func readResourceCRDs(ctx context.Context, kc *kubeClients) ([]resourceCRDRow, bool, error) {
	if kc == nil || kc.dyn == nil {
		return nil, false, resourceUnavailable()
	}
	items, truncated, err := readResourcePages(ctx, metav1.ListOptions{}, func(ctx context.Context, opts metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		list, err := kc.dyn.Resource(resourceCRDGVR).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		if list == nil {
			return nil, "", errors.New("nil CRD list")
		}
		return list.Items, list.GetContinue(), nil
	})
	if err != nil {
		return nil, false, resourceAPIError(err)
	}
	rows := make([]resourceCRDRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, resourceCRDView(item, false))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, truncated, nil
}

func (m *Module) listResourceCRDs(c *gin.Context) {
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	rows, truncated, err := readResourceCRDs(c.Request.Context(), kc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "items": rows, "state": resourceListState(truncated), "complete": !truncated, "truncated": truncated, "limit": resourceListCap, "source": "apiextensions.k8s.io/v1"})
}

func (m *Module) getResourceCRD(c *gin.Context) {
	if len(validation.IsDNS1123Subdomain(c.Param("name"))) != 0 {
		httpx.Fail(c, httpx.BadRequest("invalid CRD name"))
		return
	}
	cl, kc, ok := m.resourceKube(c)
	if !ok {
		return
	}
	if kc.dyn == nil {
		httpx.Fail(c, resourceUnavailable())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), resourceReadTimeout)
	defer cancel()
	item, err := kc.dyn.Resource(resourceCRDGVR).Get(ctx, c.Param("name"), metav1.GetOptions{})
	if err != nil {
		httpx.Fail(c, resourceAPIError(err))
		return
	}
	if item == nil {
		httpx.Fail(c, resourceAPIError(errors.New("nil CRD")))
		return
	}
	row := resourceCRDView(*item, true)
	httpx.OK(c, gin.H{"cluster_id": cl.ID, "item": row, "source": "apiextensions.k8s.io/v1"})
}

func resourceCRDView(item unstructured.Unstructured, detail bool) resourceCRDRow {
	str := func(path ...string) string { v, _, _ := unstructured.NestedString(item.Object, path...); return v }
	slice := func(path ...string) []string {
		v, _, _ := unstructured.NestedStringSlice(item.Object, path...)
		if v == nil {
			return []string{}
		}
		return v
	}
	row := resourceCRDRow{
		Name: item.GetName(), UID: string(item.GetUID()), ResourceVersion: item.GetResourceVersion(),
		Group: str("spec", "group"), Scope: str("spec", "scope"),
		Kind: str("spec", "names", "kind"), ListKind: str("spec", "names", "listKind"),
		Plural: str("spec", "names", "plural"), Singular: str("spec", "names", "singular"),
		ShortNames: slice("spec", "names", "shortNames"), Categories: slice("spec", "names", "categories"),
		StoredVersions: slice("status", "storedVersions"), Versions: []resourceCRDVersion{}, Conditions: []map[string]string{},
		ConversionStrategy: str("spec", "conversion", "strategy"),
	}
	if created := item.GetCreationTimestamp(); !created.IsZero() {
		row.CreatedAt = created.UTC().Format("2006-01-02T15:04:05Z")
	}
	if detail {
		row.SchemaProjection = "structural; omits defaults, examples, enums, descriptions and CEL expressions"
	}
	versions, _, _ := unstructured.NestedSlice(item.Object, "spec", "versions")
	// One shared budget bounds the entire detail, not each individual version.
	budget := 10000
	for _, raw := range versions {
		v, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(v, "name")
		served, _, _ := unstructured.NestedBool(v, "served")
		storage, _, _ := unstructured.NestedBool(v, "storage")
		deprecated, _, _ := unstructured.NestedBool(v, "deprecated")
		version := resourceCRDVersion{Name: name, Served: served, Storage: storage, Deprecated: deprecated}
		if detail {
			s, _, _ := unstructured.NestedMap(v, "schema", "openAPIV3Schema")
			version.Schema, version.SchemaTruncated = projectCRDSchema(s, 0, &budget)
		}
		row.Versions = append(row.Versions, version)
	}
	conditions, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		projected := map[string]string{}
		for _, key := range []string{"type", "status", "reason", "lastTransitionTime"} {
			if value, ok := c[key].(string); ok {
				projected[key] = value
			}
		}
		row.Conditions = append(row.Conditions, projected)
	}
	return row
}

// Never return whole CRD objects: webhook credentials, annotations, defaults,
// examples and CEL text can contain embedded configuration or sensitive values.
func projectCRDSchema(input map[string]any, depth int, budget *int) (map[string]any, bool) {
	if input == nil {
		return nil, false
	}
	if depth > 40 || *budget <= 0 {
		return nil, true
	}
	*budget--
	out := map[string]any{}
	truncated := false
	// Iterate only the public structural fields. Large private defaults or
	// examples cannot consume the projection budget or enter the traversal.
	for _, key := range []string{
		"type", "format", "pattern", "nullable", "required", "properties", "items", "additionalProperties",
		"allOf", "anyOf", "oneOf", "not", "uniqueItems", "exclusiveMinimum", "exclusiveMaximum",
		"minimum", "maximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties",
		"x-kubernetes-list-map-keys", "x-kubernetes-list-type", "x-kubernetes-map-type",
		"x-kubernetes-preserve-unknown-fields", "x-kubernetes-embedded-resource", "x-kubernetes-int-or-string",
	} {
		value, exists := input[key]
		if !exists {
			continue
		}
		if *budget <= 0 {
			truncated = true
			break
		}
		*budget--
		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				projected := map[string]any{}
				names := make([]string, 0, len(props))
				for name := range props {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					if *budget <= 0 {
						truncated = true
						break
					}
					*budget--
					if len(name) > 4096 {
						truncated = true
						continue
					}
					if child, ok := props[name].(map[string]any); ok {
						p, cut := projectCRDSchema(child, depth+1, budget)
						truncated = truncated || cut
						projected[name] = p
					} else {
						truncated = true
					}
				}
				out[key] = projected
			}
		case "items", "not", "additionalProperties":
			if child, ok := value.(map[string]any); ok {
				p, cut := projectCRDSchema(child, depth+1, budget)
				out[key] = p
				truncated = truncated || cut
			} else if b, ok := value.(bool); ok {
				out[key] = b
			}
		case "allOf", "anyOf", "oneOf":
			if children, ok := value.([]any); ok {
				projected := []any{}
				for _, child := range children {
					if *budget <= 0 {
						truncated = true
						break
					}
					*budget--
					if s, ok := child.(map[string]any); ok {
						p, cut := projectCRDSchema(s, depth+1, budget)
						projected = append(projected, p)
						truncated = truncated || cut
					} else {
						truncated = true
					}
				}
				out[key] = projected
			}
		case "required", "x-kubernetes-list-map-keys":
			if values, ok := value.([]any); ok {
				projected := []string{}
				for _, raw := range values {
					if *budget <= 0 {
						truncated = true
						break
					}
					*budget--
					if s, ok := raw.(string); ok && len(s) <= 4096 {
						projected = append(projected, s)
					} else {
						truncated = true
					}
				}
				out[key] = projected
			}
		case "type", "format", "pattern", "x-kubernetes-list-type", "x-kubernetes-map-type":
			if s, ok := value.(string); ok {
				if len(s) <= 4096 {
					out[key] = s
				} else {
					truncated = true
				}
			}
		case "nullable", "uniqueItems", "exclusiveMinimum", "exclusiveMaximum", "x-kubernetes-preserve-unknown-fields", "x-kubernetes-embedded-resource", "x-kubernetes-int-or-string":
			if b, ok := value.(bool); ok {
				out[key] = b
			}
		case "minimum", "maximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
			switch value.(type) {
			case int64, float64:
				out[key] = value
			}
		}
	}
	return out, truncated
}
