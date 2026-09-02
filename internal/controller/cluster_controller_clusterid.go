package controller

import (
	"context"
	"fmt"
	"io"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	components "github.com/castai/castware-operator/internal/component"
)

// This file contains the logic for discovering the CAST AI cluster ID by
// scraping it out of the agent pod logs when it cannot be obtained through
// the regular registration flow.

// extractClusterIDFromAgentLogs extracts the cluster_id from the logs of the agent container
// in the castai-agent deployment. Returns empty string and no error if the deployment doesn't exist.
func (r *ClusterReconciler) extractClusterIDFromAgentLogs(ctx context.Context, namespace string) (string, error) {
	log := r.Log.WithField("namespace", namespace)

	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: components.ComponentNameAgent}, deployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Debug("castai-agent deployment not found")
			return "", nil
		}
		return "", fmt.Errorf("failed to get castai-agent deployment: %w", err)
	}

	podList := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(deployment.Spec.Selector.MatchLabels)
	err = r.List(ctx, podList, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods for castai-agent: %w", err)
	}

	if len(podList.Items) == 0 {
		log.Debug("no pods found for castai-agent deployment")
		return "", nil
	}

	// Try to extract cluster_id from the first running pod
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Find the agent container
		var agentContainer *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "agent" {
				agentContainer = &pod.Spec.Containers[i]
				break
			}
		}

		if agentContainer == nil {
			log.WithField("pod", pod.Name).Debug("agent container not found in pod")
			continue
		}

		// Get logs from the agent container
		logReq := r.Clientset.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "agent",
			TailLines: lo.ToPtr(int64(1000)),
			// Retrieve only logs for the last minute, the agent sends snapshots every 15 seconds
			// so it should be a safe interval.
			SinceSeconds: lo.ToPtr(int64(60)),
		})

		logBytes, err := r.readLogBytes(ctx, logReq)
		if err != nil {
			log.WithError(err).WithField("pod", pod.Name).Warn("failed to read logs from agent container")
			continue
		}

		// Search for cluster_id=(uuid) pattern in logs
		clusterID := extractClusterIDFromLogs(string(logBytes))
		if clusterID != "" {
			log.WithField("clusterId", clusterID).Info("extracted cluster ID from agent logs")
			return clusterID, nil
		}
	}

	log.Debug("cluster_id not found in agent logs")
	return "", nil
}

func (r *ClusterReconciler) readLogBytes(ctx context.Context, logReq *rest.Request) ([]byte, error) {
	logStream, err := logReq.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get log stream from container: %w", err)
	}
	defer func() {
		if err := logStream.Close(); err != nil {
			r.Log.WithError(err).Warn("failed to close logs stream")
		}
	}()
	logBytes, err := io.ReadAll(logStream)
	if err != nil {
		return nil, fmt.Errorf("failed to read logs from container: %w", err)
	}
	return logBytes, nil
}

// extractClusterIDFromLogs parses logs and extracts cluster_id UUID
func extractClusterIDFromLogs(logs string) string {
	// Match cluster_id=<uuid> pattern
	matches := clusterIDRegexp.FindStringSubmatch(logs)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}
