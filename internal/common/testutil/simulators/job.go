package simulators

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SimulateJobComplete creates a Kubernetes Job (if it does not already exist)
// and patches its status sub-resource to reflect a successful completion, setting
// succeeded=1 and a "Complete" condition with status "True".
//
// This simulates the behaviour of the Kubernetes job controller in envtest
// environments where no Pods are actually scheduled and therefore Jobs never
// transition to a completed state on their own.
func SimulateJobComplete(ctx context.Context, c client.Client, name, namespace string) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "job", Image: "busybox"},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := createOrGet(ctx, c, job, "Job"); err != nil {
		return err
	}

	patch := client.MergeFrom(job.DeepCopy())

	now := metav1.NewTime(time.Now().UTC())
	job.Status.Succeeded = 1
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobSuccessCriteriaMet,
			Status:             corev1.ConditionTrue,
			Reason:             "SuccessCriteriaMet",
			Message:            "Job success criteria met",
			LastTransitionTime: now,
		},
		{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			Reason:             "Complete",
			Message:            "Job completed",
			LastTransitionTime: now,
		},
	}

	if err := c.Status().Patch(ctx, job, patch); err != nil {
		return fmt.Errorf("patching Job status: %w", err)
	}

	return nil
}
