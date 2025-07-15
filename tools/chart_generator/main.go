package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"sigs.k8s.io/yaml"
	"sort"
	"strings"

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
		rbacObjs []*unstructured.Unstructured
		saObjs   []*unstructured.Unstructured
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
			rbacObjs = append(rbacObjs, u)
		case "ServiceAccount":
			saObjs = append(saObjs, u)
		}

	}

	// Sort RBAC objects to have a repeatable code generation.
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

	if err := injectAndWrite(rbacObjs, "charts/templates/rbac.yaml", "", ""); err != nil {
		return nil, err
	}
	if err := injectAndWrite(saObjs, "charts/templates/serviceaccount.yaml", "{{- if .Values.serviceAccount.create -}}", "{{- end }}"); err != nil {
		return nil, err
	}

	return rbacObjs, nil
}

func injectAndWrite(objs []*unstructured.Unstructured, outFilePath, header, footer string) error {
	outF, err := os.Create(outFilePath)
	if err != nil {
		return err
	}
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
		injectedData, err := injectTemplating(data, obj)
		if err != nil {
			return err
		}
		if _, err := outF.Write(injectedData); err != nil {
			return err
		}
	}

	if footer != "" {
		if _, err := outF.WriteString("\n" + footer); err != nil {

		}
	}
	return nil
}

func injectTemplating(in []byte, obj *unstructured.Unstructured) ([]byte, error) {
	lines := splitLines(in)

	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) == "metadata:" {
			// determine indentation
			indentLen := countLeadingSpaces(line)
			indent := strings.Repeat(" ", indentLen)
			namespace := `"{{ .Release.Namespace }}"`
			if obj.GetNamespace() != "castai-agent" {
				namespace = obj.GetNamespace()
			}

			// emit the templated metadata block
			out = append(out, indent+"metadata:")

			// ClusterRole and ClusterRoleBinding are not namespaced
			if !strings.HasPrefix(obj.GetKind(), "Cluster") {
				out = append(out, indent+`  namespace: `+namespace)
			}

			out = append(out,
				indent+`  name: `+strings.Replace(obj.GetName(), "castware-operator", `{{ include "castware-operator.fullname" . }}`, 1),
				indent+"  labels:",
				indent+"    {{- include \"castware-operator.labels\" . | nindent 4 }}",
			)

			// skip the original metadata block
			i++
			for i < len(lines) {
				if countLeadingSpaces(lines[i]) <= indentLen {
					break
				}
				i++
			}
			continue
		}
		line = strings.Replace(line, "castware-operator-controller-manager", "{{ include \"castware-operator.fullname\" . }}-controller-manager", -1)
		out = append(out, line)
		i++
	}

	return []byte(strings.Join(out, "\n")), nil
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

func main() {
	f, err := os.Open("./dist/install.yaml")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	objs, err := parseManifests(f)
	if err != nil {
		panic(err)
	}
	for _, u := range objs {
		fmt.Printf("Kind=%s  Name=%s\n", u.GetKind(), u.GetName())
	}
}
