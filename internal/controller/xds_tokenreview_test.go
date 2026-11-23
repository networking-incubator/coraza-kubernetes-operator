/*
Copyright Coraza Kubernetes Operator contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
)

// TestXDSAuthenticatorAgainstAPIServer records how a real API server answers
// TokenReviews for a pod-bound token, a garbage token and the token of a
// deleted pod, and shows that the xDS authenticator charges the per-source
// failure limiter for rejected tokens.
func TestXDSAuthenticatorAgainstAPIServer(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	require.NoError(t, k8sClient.Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway"},
	}))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "gateway-0"},
		Spec: corev1.PodSpec{
			ServiceAccountName: "gateway",
			Containers:         []corev1.Container{{Name: "proxy", Image: "proxy"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))

	request, err := testKubeClient.CoreV1().ServiceAccounts(ns).CreateToken(ctx, "gateway", &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	podToken := request.Status.Token

	reviews := testKubeClient.AuthenticationV1().TokenReviews()
	review := func(token string) authenticationv1.TokenReviewStatus {
		t.Helper()
		result, err := reviews.Create(ctx, &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		return result.Status
	}

	live := review(podToken)
	require.True(t, live.Authenticated, "live pod token: %+v", live)
	require.Equal(t, []string{string(pod.UID)}, []string(live.User.Extra["authentication.kubernetes.io/pod-uid"]))

	garbage := review("not-a-token")
	t.Logf("garbage token review: authenticated=%t error=%q", garbage.Authenticated, garbage.Error)
	require.False(t, garbage.Authenticated)
	require.NotEmpty(t, garbage.Error, "the API server reports a rejected token with an error string")

	require.NoError(t, k8sClient.Delete(ctx, pod))
	var deleted authenticationv1.TokenReviewStatus
	require.Eventually(t, func() bool {
		deleted = review(podToken)
		return !deleted.Authenticated
	}, 30*time.Second, 250*time.Millisecond, "a deleted pod's token must stop authenticating")
	t.Logf("deleted pod token review: authenticated=%t error=%q", deleted.Authenticated, deleted.Error)
	require.NotEmpty(t, deleted.Error, "the API server reports a deleted pod's token with an error string")

	// NewServer installs the per-source failure limiter from the limits.
	limits := xds.DefaultLimits()
	limits.AuthFailureRate = 0.001
	limits.AuthFailureBurst = 2
	auth := xds.NewAuthenticator(reviews, "", time.Minute, time.Minute, 5*time.Second, 0)
	xds.NewServer(xds.NewSnapshotStore(), auth, nil, nil, limits, nil)

	for name, token := range map[string]string{"garbage": "not-a-token", "deleted pod": podToken} {
		if _, err := auth.Authenticate(ctx, token); !errors.Is(err, xds.ErrUnauthenticated) {
			t.Fatalf("%s token error = %v, want ErrUnauthenticated", name, err)
		}
	}
	if _, err := auth.Authenticate(ctx, "not-a-token"); !errors.Is(err, xds.ErrAuthRateLimited) {
		t.Fatalf("third rejected token error = %v, want ErrAuthRateLimited once both rejections spent the burst", err)
	}
}
