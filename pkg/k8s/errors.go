package k8s

import k8serrors "k8s.io/apimachinery/pkg/api/errors"

// IsTransientAPIError reports whether err is a Kubernetes API error that may
// resolve on retry.
func IsTransientAPIError(err error) bool {
	return k8serrors.IsInternalError(err) ||
		k8serrors.IsServerTimeout(err) ||
		k8serrors.IsTimeout(err) ||
		k8serrors.IsTooManyRequests(err)
}
