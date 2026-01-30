package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

var rbacKindOrder = map[string]int{
	"Role":               0,
	"ClusterRole":        1,
	"RoleBinding":        2,
	"ClusterRoleBinding": 3,
}

func parseManifests(r io.Reader) ([]*unstructured.Unstructured, error) {
	var (
		rbacObjs                []*unstructured.Unstructured
		extendedPermissionsObjs []*unstructured.Unstructured
	)

	dec := yamlutil.NewYAMLOrJSONDecoder(r, 4096)

	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if err == io.EOF {
				break
			}
			panic(err)
		}
		// skip empty docs
		if u.GetKind() == "" {
			continue
		}

		// filter by kind
		switch u.GetKind() {
		case "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
			if u.GetLabels()["castware.cast.ai/extended-permissions"] == "true" {
				extendedPermissionsObjs = append(extendedPermissionsObjs, u)
			} else {
				rbacObjs = append(rbacObjs, u)
			}

		}

	}

	// Sort RBAC objects to have a repeatable code generation.
	rbacObjs = sortRbacObjs(rbacObjs)
	extendedPermissionsObjs = sortRbacObjs(extendedPermissionsObjs)

	if err := injectAndWrite(rbacObjs, "charts/castai-castware-operator/templates/rbac.yaml", "", ""); err != nil {
		return nil, err
	}

	if err := injectAndWrite(
		extendedPermissionsObjs,
		"charts/castai-castware-operator/templates/rbac-ext.yaml",
		"{{- if .Values.extendedPermissions }}",
		"{{- end }}",
	); err != nil {
		return nil, err
	}

	return rbacObjs, nil
}
func sortRbacObjs(rbacObjs []*unstructured.Unstructured) []*unstructured.Unstructured {
	sort.Slice(rbacObjs, func(i, j int) bool {
		oi, oj := rbacObjs[i], rbacObjs[j]

		// look up kind rank (default to a high number if missing)
		ri, rj := rbacKindOrder[oi.GetKind()], rbacKindOrder[oj.GetKind()]
		if ri != rj {
			return ri < rj
		}

		// same kind → sort by metadata.name
		return oi.GetName() < oj.GetName()
	})
	return rbacObjs
}

func injectAndWrite(objs []*unstructured.Unstructured, outFilePath, header, footer string) error {
	outF, err := os.Create(outFilePath)
	if err != nil {
		return err
	}
	// nolint:errcheck
	defer outF.Close()
	if header != "" {
		if _, err := outF.WriteString(header + "\n"); err != nil {
			return err
		}
	}

	for i, obj := range objs {
		data, err := yaml.Marshal(obj.Object)
		if err != nil {
			return err
		}
		if i > 0 {
			if _, err := outF.WriteString("\n---\n"); err != nil {
				return err
			}
		}
		injectedData := injectTemplating(data, obj)
		if _, err := outF.Write(injectedData); err != nil {
			return err
		}
	}

	if footer != "" {
		if _, err := outF.WriteString("\n" + footer); err != nil {
			return err
		}
	}
	return nil
}

func injectTemplating(in []byte, obj *unstructured.Unstructured) []byte {
	// ============================================================
	// Phase 1: RBAC structural mutation
	// - Split namespaces rule
	// - Global read rule (get/list/watch)
	// - Scoped patch rule (Release.Namespace only)
	// ============================================================

	var data map[string]interface{}
	if err := yaml.Unmarshal(in, &data); err == nil {

		kind := obj.GetKind()
		if kind == "Role" || kind == "ClusterRole" {

			rules, found, err := unstructured.NestedSlice(data, "rules")
			if err == nil && found {

				var newRules []interface{}

				for _, r := range rules {
					rule, ok := r.(map[string]interface{})
					if !ok {
						newRules = append(newRules, r)
						continue
					}

					apiGroups, _ := rule["apiGroups"].([]interface{})
					resources, _ := rule["resources"].([]interface{})
					verbs, _ := rule["verbs"].([]interface{})

					// detect: core/namespaces rule with patch + other verbs
					if containsStr(apiGroups, "") &&
						len(resources) == 1 && containsStr(resources, "namespaces") &&
						containsStr(verbs, "patch") &&
						len(verbs) > 1 {

						// ---------- Rule 1: global read ----------
						readVerbs := []interface{}{}
						for _, v := range verbs {
							if s, ok := v.(string); ok && s != "patch" {
								readVerbs = append(readVerbs, s)
							}
						}

						readRule := map[string]interface{}{
							"apiGroups": apiGroups,
							"resources": resources,
							"verbs":     readVerbs,
						}

						// ---------- Rule 2: scoped patch ----------
						patchRule := map[string]interface{}{
							"apiGroups": apiGroups,
							"resources": resources,
							"verbs":     []interface{}{"patch"},
							"resourceNames": []interface{}{
								"{{ .Release.Namespace }}",
							},
						}

						newRules = append(newRules, readRule, patchRule)
						continue
					}

					newRules = append(newRules, rule)
				}

				_ = unstructured.SetNestedSlice(data, newRules, "rules")
			}
		}

		// re-marshal after RBAC mutation
		if out, err := yaml.Marshal(data); err == nil {
			in = out
		}
	}

	// ============================================================
	// Phase 2: Helm templating + metadata injection
	// ============================================================

	lines := splitLines(in)
	var out []string

	for i := 0; i < len(lines); {
		line := lines[i]

		// ---- metadata injection ----
		if strings.TrimSpace(line) == "metadata:" {
			indentLen := countLeadingSpaces(line)
			indent := strings.Repeat(" ", indentLen)

			namespace := `"{{ .Release.Namespace }}"`
			if obj.GetNamespace() != "castai-agent" {
				namespace = obj.GetNamespace()
			}

			out = append(out, indent+"metadata:")

			if !strings.HasPrefix(obj.GetKind(), "Cluster") {
				out = append(out, indent+`  namespace: `+namespace)
			}

			extendedPermissions := obj.GetLabels()["castware.cast.ai/extended-permissions"] == "true"

			out = append(out,
				indent+`  name: `+
					strings.Replace(
						obj.GetName(),
						"castware-operator",
						`{{ include "castware-operator.fullname" . }}`,
						1,
					),
				indent+"  labels:",
			)

			if extendedPermissions {
				out = append(out, indent+"    castware.cast.ai/extended-permissions: \"true\"")
			}

			out = append(out, indent+"    {{- include \"castware-operator.labels\" . | nindent 4 }}")

			// skip original metadata
			i++
			for i < len(lines) {
				if countLeadingSpaces(lines[i]) <= indentLen {
					break
				}
				i++
			}
			continue
		}

		// ---- controller-manager rename ----
		line = strings.ReplaceAll(
			line,
			"castware-operator-controller-manager",
			"{{ include \"castware-operator.fullname\" . }}-controller-manager",
		)

		out = append(out, line)
		i++
	}

	return []byte(strings.Join(out, "\n"))
}

func containsStr(list []interface{}, v string) bool {
	for _, x := range list {
		if s, ok := x.(string); ok && s == v {
			return true
		}
	}
	return false
}

func splitLines(b []byte) []string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

func countLeadingSpaces(s string) int {
	count := 0
	for _, ch := range s {
		if ch == ' ' {
			count++
		} else {
			break
		}
	}
	return count
}

func updateCRDs(crdBasePath, outFilePath string) error {
	outF, err := os.Create(outFilePath)
	if err != nil {
		return fmt.Errorf("failed to open helm CRDs file: %w", err)
	}
	crdFiles := []string{"castware.cast.ai_clusters.yaml", "castware.cast.ai_components.yaml"}
	for i, fileName := range crdFiles {
		f, err := os.ReadFile(path.Join(crdBasePath, fileName))
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", fileName, err)
		}
		if _, err := outF.WriteString(strings.TrimPrefix(string(f), "---\n")); err != nil {
			return fmt.Errorf("failed to write %s: %w", fileName, err)
		}
		if i < len(crdFiles)-1 {
			if _, err := outF.WriteString("---\n"); err != nil {
				return fmt.Errorf("failed to write separator: %w", err)
			}
		}
	}
	return nil
}

func main() {
	if err := updateCRDs("./config/crd/bases/", "./charts/castai-castware-operator/crds/crds.yaml"); err != nil {
		panic(fmt.Errorf("failed to update CRDs: %w", err))
	}
	f, err := os.Open("./dist/install.yaml")
	if err != nil {
		panic(err)
	}
	// nolint:errcheck
	defer f.Close()

	objs, err := parseManifests(f)
	if err != nil {
		panic(err)
	}
	for _, u := range objs {
		fmt.Printf("Kind=%s  Name=%s\n", u.GetKind(), u.GetName())
	}
}
