package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <file1.yaml> <file2.yaml>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Compare two Kubernetes Role or ClusterRole YAML files\n")
		os.Exit(1)
	}

	file1 := os.Args[1]
	file2 := os.Args[2]

	fmt.Printf("Comparing:\n  File 1: %s\n  File 2: %s\n\n", file1, file2)

	rules1, name1, kind1, err := parseRBACFile(file1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", file1, err)
		os.Exit(1)
	}

	rules2, name2, kind2, err := parseRBACFile(file2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", file2, err)
		os.Exit(1)
	}

	fmt.Printf("%s: %s\n", kind1, name1)
	fmt.Printf("%s: %s\n\n", kind2, name2)

	// Sort rules for consistent comparison
	sortPolicyRules(rules1)
	sortPolicyRules(rules2)

	// Compare using go-cmp
	diff := cmp.Diff(rules1, rules2,
		cmpopts.SortSlices(func(a, b string) bool { return a < b }),
		cmpopts.EquateEmpty(),
	)

	if diff == "" {
		fmt.Println("✓ No differences found - the roles are identical")
		return
	}

	fmt.Println("Differences found:")
	fmt.Println(diff)

	// Additional detailed comparison
	printDetailedDiff(rules1, rules2)
}

func parseRBACFile(filename string) ([]rbacv1.PolicyRule, string, string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to read file: %w", err)
	}

	// Try to parse as Role first
	var role rbacv1.Role
	if err := yaml.Unmarshal(data, &role); err == nil && role.Kind == "Role" {
		return role.Rules, role.Name, "Role", nil
	}

	// Try to parse as ClusterRole
	var clusterRole rbacv1.ClusterRole
	if err := yaml.Unmarshal(data, &clusterRole); err == nil && clusterRole.Kind == "ClusterRole" {
		return clusterRole.Rules, clusterRole.Name, "ClusterRole", nil
	}

	return nil, "", "", fmt.Errorf("file is not a valid Role or ClusterRole")
}

func sortPolicyRules(rules []rbacv1.PolicyRule) {
	for i := range rules {
		sort.Strings(rules[i].Verbs)
		sort.Strings(rules[i].APIGroups)
		sort.Strings(rules[i].Resources)
		sort.Strings(rules[i].ResourceNames)
	}
	sort.Slice(rules, func(i, j int) bool {
		if len(rules[i].APIGroups) > 0 && len(rules[j].APIGroups) > 0 {
			if rules[i].APIGroups[0] != rules[j].APIGroups[0] {
				return rules[i].APIGroups[0] < rules[j].APIGroups[0]
			}
		}
		if len(rules[i].Resources) > 0 && len(rules[j].Resources) > 0 {
			return rules[i].Resources[0] < rules[j].Resources[0]
		}
		return false
	})
}

func printDetailedDiff(rules1, rules2 []rbacv1.PolicyRule) {
	fmt.Println("\n=== Detailed Analysis ===\n")

	// Create maps for easier lookup
	rulesMap1 := make(map[string]rbacv1.PolicyRule)
	rulesMap2 := make(map[string]rbacv1.PolicyRule)

	for _, rule := range rules1 {
		key := ruleKey(rule)
		rulesMap1[key] = rule
	}

	for _, rule := range rules2 {
		key := ruleKey(rule)
		rulesMap2[key] = rule
	}

	// Find rules only in file 1
	var onlyInFile1 []rbacv1.PolicyRule
	for key, rule := range rulesMap1 {
		if _, exists := rulesMap2[key]; !exists {
			onlyInFile1 = append(onlyInFile1, rule)
		}
	}

	// Find rules only in file 2
	var onlyInFile2 []rbacv1.PolicyRule
	for key, rule := range rulesMap2 {
		if _, exists := rulesMap1[key]; !exists {
			onlyInFile2 = append(onlyInFile2, rule)
		}
	}

	if len(onlyInFile1) > 0 {
		fmt.Println("Rules only in File 1:")
		for _, rule := range onlyInFile1 {
			printRule(rule)
		}
		fmt.Println()
	}

	if len(onlyInFile2) > 0 {
		fmt.Println("Rules only in File 2:")
		for _, rule := range onlyInFile2 {
			printRule(rule)
		}
		fmt.Println()
	}

	// Check for permission differences
	checkPermissionDifferences(rules1, rules2)
}

func ruleKey(rule rbacv1.PolicyRule) string {
	return fmt.Sprintf("%v-%v-%v", rule.APIGroups, rule.Resources, rule.ResourceNames)
}

func printRule(rule rbacv1.PolicyRule) {
	fmt.Printf("  - APIGroups: %v\n", rule.APIGroups)
	fmt.Printf("    Resources: %v\n", rule.Resources)
	if len(rule.ResourceNames) > 0 {
		fmt.Printf("    ResourceNames: %v\n", rule.ResourceNames)
	}
	fmt.Printf("    Verbs: %v\n", rule.Verbs)
}

func checkPermissionDifferences(rules1, rules2 []rbacv1.PolicyRule) {
	perms1 := extractPermissions(rules1)
	perms2 := extractPermissions(rules2)

	var addedPerms, removedPerms []string

	for perm := range perms2 {
		if !perms1[perm] {
			addedPerms = append(addedPerms, perm)
		}
	}

	for perm := range perms1 {
		if !perms2[perm] {
			removedPerms = append(removedPerms, perm)
		}
	}

	if len(addedPerms) > 0 {
		sort.Strings(addedPerms)
		fmt.Println("Permissions added in File 2:")
		for _, perm := range addedPerms {
			fmt.Printf("  + %s\n", perm)
		}
		fmt.Println()
	}

	if len(removedPerms) > 0 {
		sort.Strings(removedPerms)
		fmt.Println("Permissions removed from File 1:")
		for _, perm := range removedPerms {
			fmt.Printf("  - %s\n", perm)
		}
		fmt.Println()
	}
}

func extractPermissions(rules []rbacv1.PolicyRule) map[string]bool {
	perms := make(map[string]bool)

	for _, rule := range rules {
		for _, apiGroup := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					perm := fmt.Sprintf("%s:%s:%s", verb, apiGroup, resource)
					if apiGroup == "" {
						perm = fmt.Sprintf("%s:core:%s", verb, resource)
					}
					perms[perm] = true
				}
			}
		}
	}

	return perms
}
