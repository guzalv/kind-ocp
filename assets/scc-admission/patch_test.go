package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBuildPatch(t *testing.T) {
	uid := int64(1001)
	sc := func(user, group, fs *int64) *podSecurityContextLike {
		return &podSecurityContextLike{RunAsUser: user, RunAsGroup: group, FSGroup: fs}
	}
	i := func(v int64) *int64 { return &v }

	tests := []struct {
		name string
		spec podSpecLike
		want []patchOp
	}{
		{
			name: "no securityContext creates it and assigns all three",
			spec: podSpecLike{},
			want: []patchOp{
				{Op: "add", Path: "/spec/securityContext", Value: map[string]any{}},
				{Op: "add", Path: "/spec/securityContext/runAsUser", Value: uid},
				{Op: "add", Path: "/spec/securityContext/runAsGroup", Value: uid},
				{Op: "add", Path: "/spec/securityContext/fsGroup", Value: uid},
			},
		},
		{
			name: "securityContext without runAsUser assigns the three in place",
			spec: podSpecLike{SecurityContext: sc(nil, nil, nil)},
			want: []patchOp{
				{Op: "add", Path: "/spec/securityContext/runAsUser", Value: uid},
				{Op: "add", Path: "/spec/securityContext/runAsGroup", Value: uid},
				{Op: "add", Path: "/spec/securityContext/fsGroup", Value: uid},
			},
		},
		{
			name: "an explicit runAsUser mutates nothing",
			spec: podSpecLike{SecurityContext: sc(i(1234), nil, nil)},
			want: nil,
		},
		{
			name: "an explicit runAsUser wins even with the others unset",
			spec: podSpecLike{SecurityContext: sc(i(0), nil, nil)},
			want: nil,
		},
		{
			name: "an explicit runAsGroup is kept, the unset fields are assigned",
			spec: podSpecLike{SecurityContext: sc(nil, i(2000), nil)},
			want: []patchOp{
				{Op: "add", Path: "/spec/securityContext/runAsUser", Value: uid},
				{Op: "add", Path: "/spec/securityContext/fsGroup", Value: uid},
			},
		},
		{
			name: "an explicit fsGroup is kept, the unset fields are assigned",
			spec: podSpecLike{SecurityContext: sc(nil, nil, i(3000))},
			want: []patchOp{
				{Op: "add", Path: "/spec/securityContext/runAsUser", Value: uid},
				{Op: "add", Path: "/spec/securityContext/runAsGroup", Value: uid},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPatch(tt.spec, uid)
			if len(got) != len(tt.want) {
				t.Fatalf("buildPatch = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i].Op != tt.want[i].Op || got[i].Path != tt.want[i].Path || !reflect.DeepEqual(got[i].Value, tt.want[i].Value) {
					t.Errorf("op %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// A null securityContext decodes to a nil pointer and must take the
// "absent" path: the patch creates it before adding fields.
func TestBuildPatchNullSecurityContext(t *testing.T) {
	var spec podSpecLike
	if err := json.Unmarshal([]byte(`{"spec":{"securityContext":null}}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SecurityContext != nil {
		t.Fatal("a null securityContext decoded to a non-nil value")
	}
	ops := buildPatch(spec, 1001)
	if len(ops) != 4 || ops[0].Path != "/spec/securityContext" {
		t.Fatalf("buildPatch = %+v, want the securityContext created then three fields assigned", ops)
	}
}

func TestMutateReview(t *testing.T) {
	review := func(podSpec string) string {
		return `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview",` +
			`"request":{"uid":"abc-123","object":{"apiVersion":"v1","kind":"Pod",` +
			`"spec":` + podSpec + `}}}`
	}

	t.Run("assigns the UID to a pod without one", func(t *testing.T) {
		got, err := mutateReview([]byte(review(`{"containers":[{"name":"scanner"}]}`)), 1001)
		if err != nil {
			t.Fatal(err)
		}
		if got.APIVersion != "admission.k8s.io/v1" || got.Kind != "AdmissionReview" {
			t.Errorf("review = %s/%s, want admission.k8s.io/v1/AdmissionReview", got.APIVersion, got.Kind)
		}
		if got.Response.UID != "abc-123" || !got.Response.Allowed {
			t.Errorf("response = %+v, want the request's uid echoed and allowed", got.Response)
		}
		if got.Response.PatchType != "JSONPatch" {
			t.Fatalf("patchType = %q, want JSONPatch", got.Response.PatchType)
		}
		patch, err := base64.StdEncoding.DecodeString(got.Response.Patch)
		if err != nil {
			t.Fatalf("the patch is not standard base64: %v", err)
		}
		var ops []patchOp
		if err := json.Unmarshal(patch, &ops); err != nil {
			t.Fatalf("the patch is not a JSON Patch: %v", err)
		}
		// The pod carries no securityContext, so the patch creates it
		// and then assigns the three fields.
		if len(ops) != 4 || ops[0].Path != "/spec/securityContext" {
			t.Fatalf("patch ops = %+v, want the securityContext created then three fields assigned", ops)
		}
		// The JSON round-trip decodes the numbers as float64.
		for _, op := range ops[1:] {
			if op.Value != float64(1001) {
				t.Errorf("op %s value = %v, want the configured UID", op.Path, op.Value)
			}
		}
	})

	t.Run("a pod with an explicit UID is allowed unmutated", func(t *testing.T) {
		got, err := mutateReview([]byte(review(`{"securityContext":{"runAsUser":1234}}`)), 1001)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Response.Allowed || got.Response.Patch != "" || got.Response.PatchType != "" {
			t.Errorf("response = %+v, want allowed with no patch", got.Response)
		}
	})

	t.Run("an unknown apiVersion defaults to v1 and is echoed", func(t *testing.T) {
		body := strings.Replace(review(`{}`), `"apiVersion":"admission.k8s.io/v1"`, `"apiVersion":""`, 1)
		got, err := mutateReview([]byte(body), 1001)
		if err != nil {
			t.Fatal(err)
		}
		if got.APIVersion != "admission.k8s.io/v1" {
			t.Errorf("apiVersion = %q, want the v1 default", got.APIVersion)
		}
	})

	t.Run("garbage is an error, never a silent allow", func(t *testing.T) {
		for _, body := range []string{`{`, `{"request":null}`, `not json at all`} {
			if _, err := mutateReview([]byte(body), 1001); err == nil {
				t.Errorf("mutateReview(%q) succeeded, want an error", body)
			}
		}
	})
}

// The HTTP layer is thin; these pin its contract: health for the probe,
// POST-only mutation, and 400 — not a silent allow — for a bad body.
func TestServeMutation(t *testing.T) {
	server := httptest.NewServer(webhookHandler(1001))
	defer server.Close()

	t.Run("healthz answers ok", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/healthz = %s, want 200", resp.Status)
		}
	})

	t.Run("mutate requires POST", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/mutate")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /mutate = %s, want 405", resp.Status)
		}
	})

	t.Run("mutate answers a review with the patch", func(t *testing.T) {
		resp, err := http.Post(server.URL+"/mutate", "application/json",
			strings.NewReader(`{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview",`+
				`"request":{"uid":"u1","object":{"spec":{"containers":[{"name":"c"}]}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /mutate = %s, want 200", resp.Status)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		var got admissionReviewResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Response.UID != "u1" || !got.Response.Allowed || got.Response.Patch == "" {
			t.Errorf("response = %+v, want the uid echoed, allowed, and patched", got.Response)
		}
	})

	t.Run("a bad body is a 400", func(t *testing.T) {
		resp, err := http.Post(server.URL+"/mutate", "application/json", strings.NewReader("garbage"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST garbage = %s, want 400", resp.Status)
		}
	})
}

func TestRunAsUserFromEnv(t *testing.T) {
	t.Setenv(envRunAsUser, "1001")
	if got, err := runAsUserFromEnv(); err != nil || got != 1001 {
		t.Fatalf("runAsUserFromEnv = (%d, %v), want (1001, nil)", got, err)
	}
	for _, value := range []string{"", "abc", "0", "-1"} {
		t.Setenv(envRunAsUser, value)
		if _, err := runAsUserFromEnv(); err == nil {
			t.Errorf("runAsUserFromEnv(%q) succeeded, want an error", value)
		}
	}
	if err := os.Unsetenv(envRunAsUser); err != nil {
		t.Fatal(err)
	}
	if _, err := runAsUserFromEnv(); err == nil {
		t.Error("runAsUserFromEnv with the variable unset succeeded, want an error")
	}
}
