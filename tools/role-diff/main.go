package main

import (
	"fmt"
	"os"
	"sort"

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

	// Simple comparison
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
	// Create maps for easier lookup - use resource key (without verbs)
	rulesMap1 := make(map[string][]rbacv1.PolicyRule)
	rulesMap2 := make(map[string][]rbacv1.PolicyRule)

	for _, rule := range rules1 {
		key := ruleKey(rule)
		rulesMap1[key] = append(rulesMap1[key], rule)
	}

	for _, rule := range rules2 {
		key := ruleKey(rule)
		rulesMap2[key] = append(rulesMap2[key], rule)
	}

	// Find rules only in file 1 (based on resources, not verbs)
	var onlyInFile1 []rbacv1.PolicyRule
	for key, rules := range rulesMap1 {
		if _, exists := rulesMap2[key]; !exists {
			onlyInFile1 = append(onlyInFile1, rules...)
		}
	}

	// Find rules only in file 2 (based on resources, not verbs)
	var onlyInFile2 []rbacv1.PolicyRule
	for key, rules := range rulesMap2 {
		if _, exists := rulesMap1[key]; !exists {
			onlyInFile2 = append(onlyInFile2, rules...)
		}
	}

	// Find verb differences for rules that exist in both files
	type verbDiff struct {
		rule         rbacv1.PolicyRule
		missingVerbs []string
		extraVerbs   []string
	}

	var missingInFile1 []verbDiff
	var missingInFile2 []verbDiff

	for key := range rulesMap1 {
		if rules2, exists := rulesMap2[key]; exists {
			rules1 := rulesMap1[key]

			// Collect all verbs from both rule sets
			verbs1 := make(map[string]bool)
			verbs2 := make(map[string]bool)

			for _, rule := range rules1 {
				for _, verb := range rule.Verbs {
					verbs1[verb] = true
				}
			}

			for _, rule := range rules2 {
				for _, verb := range rule.Verbs {
					verbs2[verb] = true
				}
			}

			// Find verbs missing in file 1 (exist in file 2 but not in file 1)
			var missing1 []string
			for verb := range verbs2 {
				if !verbs1[verb] {
					missing1 = append(missing1, verb)
				}
			}

			// Find verbs missing in file 2 (exist in file 1 but not in file 2)
			var missing2 []string
			for verb := range verbs1 {
				if !verbs2[verb] {
					missing2 = append(missing2, verb)
				}
			}

			if len(missing1) > 0 {
				sort.Strings(missing1)
				// Use the first rule as template
				templateRule := rules1[0]
				missingInFile1 = append(missingInFile1, verbDiff{
					rule:         templateRule,
					missingVerbs: missing1,
				})
			}

			if len(missing2) > 0 {
				sort.Strings(missing2)
				// Use the first rule as template
				templateRule := rules2[0]
				missingInFile2 = append(missingInFile2, verbDiff{
					rule:         templateRule,
					missingVerbs: missing2,
				})
			}
		}
	}

	hasOutput := false

	if len(onlyInFile1) > 0 {
		fmt.Println("Rules only in File 1:")
		sort.Slice(onlyInFile1, func(i, j int) bool {
			if len(onlyInFile1[i].APIGroups) > 0 && len(onlyInFile1[j].APIGroups) > 0 {
				return onlyInFile1[i].APIGroups[0] < onlyInFile1[j].APIGroups[0]
			}
			return false
		})
		for _, rule := range onlyInFile1 {
			printRule(rule)
		}
		fmt.Println()
		hasOutput = true
	}

	if len(onlyInFile2) > 0 {
		fmt.Println("Rules only in File 2:")
		sort.Slice(onlyInFile2, func(i, j int) bool {
			if len(onlyInFile2[i].APIGroups) > 0 && len(onlyInFile2[j].APIGroups) > 0 {
				return onlyInFile2[i].APIGroups[0] < onlyInFile2[j].APIGroups[0]
			}
			return false
		})
		for _, rule := range onlyInFile2 {
			printRule(rule)
		}
		fmt.Println()
		hasOutput = true
	}

	if len(missingInFile1) > 0 {
		fmt.Println("Rules missing in File 1:")
		for _, diff := range missingInFile1 {
			printRuleWithVerbs(diff.rule, diff.missingVerbs)
		}
		fmt.Println()
		hasOutput = true
	}

	if len(missingInFile2) > 0 {
		fmt.Println("Rules missing in File 2:")
		for _, diff := range missingInFile2 {
			printRuleWithVerbs(diff.rule, diff.missingVerbs)
		}
		hasOutput = true
	}

	if !hasOutput {
		fmt.Println("✓ No differences found - both files are identical")
	}
}

func ruleKey(rule rbacv1.PolicyRule) string {
	return fmt.Sprintf("%v-%v-%v-%v", rule.APIGroups, rule.Resources, rule.ResourceNames, rule.NonResourceURLs)
}

func ruleKeyWithVerbs(rule rbacv1.PolicyRule) string {
	return fmt.Sprintf("%v-%v-%v-%v-%v", rule.APIGroups, rule.Resources, rule.ResourceNames, rule.NonResourceURLs, rule.Verbs)
}

func verbSetsEqual(rules1, rules2 []rbacv1.PolicyRule) bool {
	if len(rules1) != len(rules2) {
		return false
	}

	// Collect all verbs from both rule sets
	verbs1 := make(map[string]bool)
	verbs2 := make(map[string]bool)

	for _, rule := range rules1 {
		for _, verb := range rule.Verbs {
			verbs1[verb] = true
		}
	}

	for _, rule := range rules2 {
		for _, verb := range rule.Verbs {
			verbs2[verb] = true
		}
	}

	// Check if verb sets are equal
	if len(verbs1) != len(verbs2) {
		return false
	}

	for verb := range verbs1 {
		if !verbs2[verb] {
			return false
		}
	}

	return true
}

func printRule(rule rbacv1.PolicyRule) {
	if len(rule.NonResourceURLs) > 0 {
		fmt.Printf("  - NonResourceURLs: %v\n", rule.NonResourceURLs)
		fmt.Printf("    Verbs: %v\n", rule.Verbs)
		return
	}
	fmt.Printf("  - APIGroups: %v\n", rule.APIGroups)
	fmt.Printf("    Resources: %v\n", rule.Resources)
	if len(rule.ResourceNames) > 0 {
		fmt.Printf("    ResourceNames: %v\n", rule.ResourceNames)
	}
	fmt.Printf("    Verbs: %v\n", rule.Verbs)
}

func printRuleWithVerbs(rule rbacv1.PolicyRule, verbs []string) {
	if len(rule.NonResourceURLs) > 0 {
		fmt.Printf("  - nonResourceURLs: %v\n", rule.NonResourceURLs)
		fmt.Printf("    verbs: %v\n", verbs)
		return
	}
	fmt.Printf("  - apiGroups: %v\n", rule.APIGroups)
	fmt.Printf("    resources: %v\n", rule.Resources)
	if len(rule.ResourceNames) > 0 {
		fmt.Printf("    resourceNames: %v\n", rule.ResourceNames)
	}
	fmt.Printf("    verbs: %v\n", verbs)
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
		// Handle nonResourceURLs
		if len(rule.NonResourceURLs) > 0 {
			for _, url := range rule.NonResourceURLs {
				for _, verb := range rule.Verbs {
					perm := fmt.Sprintf("%s:nonResourceURL:%s", verb, url)
					perms[perm] = true
				}
			}
			continue
		}

		// Handle regular resources
		for _, apiGroup := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					perm := fmt.Sprintf("%s:%s:%s", verb, apiGroup, resource)
					if apiGroup == "" {
						perm = fmt.Sprintf("%s:core:%s", verb, resource)
					}
					// If resourceNames are specified, include them in the permission string
					// to distinguish restricted permissions from unrestricted ones
					if len(rule.ResourceNames) > 0 {
						sort.Strings(rule.ResourceNames)
						perm = fmt.Sprintf("%s[%s]", perm, rule.ResourceNames[0])
					}
					perms[perm] = true
				}
			}
		}
	}

	return perms
}
