package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxReviewBytes caps one request body. Admission reviews carry a Pod
// plus its metadata; four megabytes is far past anything the apiserver
// sends and small enough that a stuck client cannot pin memory.
const maxReviewBytes = 4 << 20

// patchTypeJSONPatch is the AdmissionReview patchType this webhook
// speaks; the apiserver applies no other kind.
const patchTypeJSONPatch = "JSONPatch"

// The admission protocol, hand-rolled down to the fields the webhook
// reads and writes: the standalone module inside the image carries no
// dependencies (stdlib only), so k8s.io/api cannot be imported here.
// Anything the apiserver sends beyond these fields is ignored, never
// rejected — forward compatibility is how admission versions survive.
type admissionReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Request    *admissionRequest `json:"request"`
}

type admissionRequest struct {
	UID    string  `json:"uid"`
	Object podLike `json:"object"`
}

// podLike is the slice of a Pod the patch logic reads.
type podLike struct {
	Spec podSpecLike `json:"spec"`
}

type podSpecLike struct {
	SecurityContext *podSecurityContextLike `json:"securityContext"`
}

type podSecurityContextLike struct {
	RunAsUser  *int64 `json:"runAsUser"`
	RunAsGroup *int64 `json:"runAsGroup"`
	FSGroup    *int64 `json:"fsGroup"`
}

// patchOp is one RFC 6902 JSON Patch operation.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type admissionReviewResponse struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Response   admissionResponse `json:"response"`
}

type admissionResponse struct {
	UID       string `json:"uid"`
	Allowed   bool   `json:"allowed"`
	PatchType string `json:"patchType,omitempty"`
	Patch     string `json:"patch,omitempty"`
}

// buildPatch returns the JSON Patch operations that assign uid to a pod
// that runs non-root without a UID of its own, or nil when the pod
// already carries one. Ported from the PoC's scc-shim-webhook.py with
// its semantics intact: an explicit runAsUser is a choice by the pod's
// author and mutates nothing (runAsGroup and fsGroup included); an
// unset runAsUser gets runAsUser, runAsGroup, and fsGroup all set to
// uid, each only where the pod has not set its own. OpenShift's
// restricted-v2 SCC assigns the three the same way.
func buildPatch(spec podSpecLike, uid int64) []patchOp {
	sc := spec.SecurityContext
	if sc != nil && sc.RunAsUser != nil {
		return nil
	}
	var ops []patchOp
	if sc == nil {
		// The parent must exist before its fields can be added.
		ops = append(ops, patchOp{Op: "add", Path: "/spec/securityContext", Value: map[string]any{}})
	}
	ops = append(ops, patchOp{Op: "add", Path: "/spec/securityContext/runAsUser", Value: uid})
	if sc == nil || sc.RunAsGroup == nil {
		ops = append(ops, patchOp{Op: "add", Path: "/spec/securityContext/runAsGroup", Value: uid})
	}
	if sc == nil || sc.FSGroup == nil {
		ops = append(ops, patchOp{Op: "add", Path: "/spec/securityContext/fsGroup", Value: uid})
	}
	return ops
}

// mutateReview decodes one AdmissionReview and builds the response
// review: the request's uid echoed, allowed, and — when the pod needs a
// UID — the base64-encoded JSONPatch that assigns it. A body that is
// not a review carrying a request is an error the caller reports as a
// 400, which the apiserver counts as a webhook error and handles
// through the configured failurePolicy: never a silent allow. A review
// that carries a request but no object is answered allowed with a
// patch for the zero-value pod spec: the wiring's rules select pod
// CREATE only, where the apiserver always sends the object, so this
// shape cannot occur on a real call — it is stated here to pin the
// protocol shape exactly.
func mutateReview(body []byte, uid int64) (admissionReviewResponse, error) {
	var review admissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		return admissionReviewResponse{}, fmt.Errorf("decoding the AdmissionReview: %w", err)
	}
	if review.Request == nil {
		return admissionReviewResponse{}, errors.New("the AdmissionReview carries no request")
	}
	response := admissionResponse{UID: review.Request.UID, Allowed: true}
	if ops := buildPatch(review.Request.Object.Spec, uid); len(ops) > 0 {
		patch, err := json.Marshal(ops)
		if err != nil {
			return admissionReviewResponse{}, fmt.Errorf("encoding the patch: %w", err)
		}
		response.PatchType = patchTypeJSONPatch
		response.Patch = base64.StdEncoding.EncodeToString(patch)
	}
	apiVersion := review.APIVersion
	if apiVersion == "" {
		apiVersion = "admission.k8s.io/v1"
	}
	return admissionReviewResponse{
		APIVersion: apiVersion,
		Kind:       "AdmissionReview",
		Response:   response,
	}, nil
}

// serveMutation answers one /mutate call.
func serveMutation(w http.ResponseWriter, r *http.Request, uid int64) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReviewBytes))
	if err != nil {
		http.Error(w, "reading the request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	response, err := mutateReview(body, uid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
