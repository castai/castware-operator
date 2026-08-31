// Package migrationgate implements the umbrella / individual charts mutual
// exclusivity gate. The umbrella chart (castai-umbrella) renders the same
// workloads as the individual component charts (castai-agent, spot-handler,
// cluster-controller). Running both produces duplicate Deployments, duplicate
// CRDs and conflicting Helm ownership, so any ambiguity resolves to blocking
// rather than double-installing.
//
// The gate is fail-safe: a helm lookup that returns driver.ErrReleaseNotFound
// means "not present", while any other error (helm unreachable, permission
// denied, ...) is treated as "present/unknown" so that the gate blocks rather
// than risks a double install.
package migrationgate

import (
	"context"
	"errors"
	"fmt"

	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/castai/castware-operator/internal/castai"
	components "github.com/castai/castware-operator/internal/component"
	"github.com/castai/castware-operator/internal/helm"
)

// Subcomponents are the individual component names whose charts overlap with
// the umbrella chart's rendered workloads. Order is stable so error messages
// and tests are deterministic.
var Subcomponents = []string{
	components.ComponentNameAgent,
	components.ComponentNameSpotHandler,
	components.ComponentNameClusterController,
}

// IsUmbrellaOrSubcomponent reports whether name is the umbrella component or
// one of the individual components whose chart overlaps with the umbrella. The
// mutual-exclusivity gate only applies to these; any other component renders
// disjoint workloads and cannot conflict.
func IsUmbrellaOrSubcomponent(name string) bool {
	if name == components.ComponentNameUmbrella {
		return true
	}
	for _, sub := range Subcomponents {
		if name == sub {
			return true
		}
	}
	return false
}

// Names holds the Mothership-resolved release names used to probe helm for the
// umbrella release and each overlapping sub-component.
type Names struct {
	UmbrellaReleaseName string
	// SubcomponentReleases maps each present sub-component's release name.
	SubcomponentReleases map[string]string
}

// ResolveUmbrellaReleaseName queries Mothership for the umbrella component's
// release name. It is a leaner alternative to ResolveNames for call sites that
// only need to probe whether the umbrella is installed. Falls back to the
// umbrella component name when Mothership returns an empty release name,
// matching getReleaseName semantics.
func ResolveUmbrellaReleaseName(ctx context.Context, c castai.CastAIClient) (string, error) {
	umbrella, err := c.GetComponentByName(ctx, components.ComponentNameUmbrella)
	if err != nil {
		return "", err
	}
	if umbrella.ReleaseName == "" {
		return components.ComponentNameUmbrella, nil
	}
	return umbrella.ReleaseName, nil
}

// ResolveNames queries Mothership for the umbrella and sub-component release
// names. The umbrella release name is required and its lookup failure is
// surfaced as an error.
//
// Sub-component resolution is fail-safe: a sub-component Mothership does not
// know about (castai.ErrNotFound) is genuinely absent and is skipped — there is
// no chart to probe. Any other error (timeout, 5xx, auth, ...) means "unknown",
// not "absent"; to keep the gate from silently passing when it cannot see the
// sub-component releases, such errors are recorded and surfaced as an error if
// resolution ends with no resolvable sub-components. This prevents an
// unreachable-Mothership outage from emptying the release map and letting an
// umbrella install proceed over individual releases that are actually present.
func ResolveNames(ctx context.Context, c castai.CastAIClient) (*Names, error) {
	umbrella, err := c.GetComponentByName(ctx, components.ComponentNameUmbrella)
	if err != nil {
		return nil, err
	}
	if umbrella.ReleaseName == "" {
		// Fall back to the component name, matching getReleaseName semantics.
		umbrella.ReleaseName = components.ComponentNameUmbrella
	}

	names := &Names{
		UmbrellaReleaseName:  umbrella.ReleaseName,
		SubcomponentReleases: map[string]string{},
	}
	var resolveErrs []error
	for _, sub := range Subcomponents {
		mc, err := c.GetComponentByName(ctx, sub)
		if err != nil {
			if errors.Is(err, castai.ErrNotFound) {
				// Mothership has no record of this component: there is no chart
				// to probe, so skipping is safe and does not weaken the gate.
				continue
			}
			// Unknown resolution failure: record it so resolution can fail
			// rather than return an empty map that would let the gate pass.
			resolveErrs = append(resolveErrs, fmt.Errorf("resolve %s: %w", sub, err))
			continue
		}
		if mc.ReleaseName == "" {
			mc.ReleaseName = sub
		}
		names.SubcomponentReleases[sub] = mc.ReleaseName
	}

	// If at least one sub-component resolved, the helm probe has something to
	// inspect and the gate can block on a present release. If none resolved and
	// we hit unknown errors, fail closed: the gate must not pass when it cannot
	// see the individual releases it is meant to guard against.
	if len(names.SubcomponentReleases) == 0 && len(resolveErrs) > 0 {
		return nil, fmt.Errorf("resolve sub-component release names: %w", errors.Join(resolveErrs...))
	}
	return names, nil
}

// UmbrellaInstalled reports whether the umbrella helm release is present in the
// given namespace. Fail-safe: any non-not-found error is treated as present so
// the gate blocks rather than risks a double install.
func UmbrellaInstalled(hc helm.Client, namespace, umbrellaReleaseName string) bool {
	if umbrellaReleaseName == "" {
		return false
	}
	return releasePresent(hc, namespace, umbrellaReleaseName)
}

// InstalledSubcomponents returns the sub-component names whose helm releases
// are currently present in the given namespace, in Subcomponents order.
// Fail-safe: a sub-component whose lookup returns a non-not-found error is
// treated as present, again so the gate blocks.
func InstalledSubcomponents(hc helm.Client, namespace string, releases map[string]string) []string {
	var present []string
	for _, sub := range Subcomponents {
		releaseName, ok := releases[sub]
		if !ok || releaseName == "" {
			continue
		}
		if releasePresent(hc, namespace, releaseName) {
			present = append(present, sub)
		}
	}
	return present
}

// releasePresent reports whether a helm release exists, treating
// driver.ErrReleaseNotFound as absent and any other error as present
// (fail-safe toward blocking).
func releasePresent(hc helm.Client, namespace, releaseName string) bool {
	_, err := hc.GetRelease(helm.GetReleaseOptions{
		Namespace:   namespace,
		ReleaseName: releaseName,
	})
	if err == nil {
		return true
	}
	return !errors.Is(err, driver.ErrReleaseNotFound)
}
