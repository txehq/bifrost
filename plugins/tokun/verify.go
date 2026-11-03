package tokun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	routingv1 "github.com/capsohq/txe/tokun/proto/gen/go/tokun/routing/v1"
)

// The request headers that carry a signed route plan. Both values are the standard base64
// (RFC 4648 section 4, padded) of the serialized message.
const (
	HeaderRoutePlan          = "tk-route-plan"     // tokun.routing.v1.RoutePlan
	HeaderRoutePlanSignature = "tk-route-plan-sig" // tokun.routing.v1.RoutePlanSignature
)

// rejection names why a route plan was refused. It is the code of the 401 body, so the
// gateway can tell a refused plan from a refused virtual key.
type rejection string

const (
	rejectMissing      rejection = "route_plan_missing"
	rejectMalformed    rejection = "route_plan_malformed"
	rejectUnknownKey   rejection = "signing_key_unknown"
	rejectSignature    rejection = "signature_invalid"
	rejectExpired      rejection = "route_plan_expired"
	rejectPlanHash     rejection = "plan_hash_mismatch"
	rejectRequestID    rejection = "request_id_mismatch"
	rejectBodyBytes    rejection = "body_bytes_mismatch"
	rejectEmptyPlan    rejection = "route_plan_empty"
	rejectSendIDReused rejection = "send_id_reused"
)

// refusal is a failed verification: the check that failed and what it saw.
type refusal struct {
	reason rejection
	detail string
}

// verifiedPlan is a route plan whose every check passed.
type verifiedPlan struct {
	plan   *routingv1.RoutePlan
	claims *routingv1.RoutePlanClaims
}

// verify checks a request's route plan headers against the signing keys and the pod's
// send_id ledger, in this order: the signature is valid for a key the plugin accepts, the
// plan has not expired, the plan bytes hash to the signed plan_sha256, the plan's request_id
// is the signed one, the body is as long as signed, the plan lists at least one key, and the
// send_id is new on this pod. The send_id is recorded only when everything else passed.
func (p *Plugin) verify(planHeader, signatureHeader string, bodyBytes int, now time.Time) (*verifiedPlan, *refusal) {
	if planHeader == "" || signatureHeader == "" {
		return nil, &refusal{rejectMissing, fmt.Sprintf("both %s and %s are required", HeaderRoutePlan, HeaderRoutePlanSignature)}
	}
	planBytes, err := base64.StdEncoding.DecodeString(planHeader)
	if err != nil {
		return nil, &refusal{rejectMalformed, fmt.Sprintf("%s is not standard base64: %v", HeaderRoutePlan, err)}
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(signatureHeader)
	if err != nil {
		return nil, &refusal{rejectMalformed, fmt.Sprintf("%s is not standard base64: %v", HeaderRoutePlanSignature, err)}
	}
	var signature routingv1.RoutePlanSignature
	if err := proto.Unmarshal(signatureBytes, &signature); err != nil {
		return nil, &refusal{rejectMalformed, fmt.Sprintf("%s is not a RoutePlanSignature: %v", HeaderRoutePlanSignature, err)}
	}
	var claims routingv1.RoutePlanClaims
	if err := proto.Unmarshal(signature.GetClaims(), &claims); err != nil {
		return nil, &refusal{rejectMalformed, fmt.Sprintf("the signed claims are not a RoutePlanClaims: %v", err)}
	}

	key, ok := p.keys[claims.GetKeyId()]
	if !ok {
		return nil, &refusal{rejectUnknownKey, fmt.Sprintf("signing key %q is not accepted", claims.GetKeyId())}
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(signature.GetClaims())
	if !hmac.Equal(mac.Sum(nil), signature.GetMac()) {
		return nil, &refusal{rejectSignature, fmt.Sprintf("the mac does not match the claims under key %q", claims.GetKeyId())}
	}

	if claims.GetExpiresAt() == nil {
		return nil, &refusal{rejectExpired, "the claims carry no expires_at"}
	}
	expiresAt := claims.GetExpiresAt().AsTime()
	if !now.Before(expiresAt) {
		return nil, &refusal{rejectExpired, fmt.Sprintf("expired at %s", expiresAt.Format(time.RFC3339Nano))}
	}

	planHash := sha256.Sum256(planBytes)
	if !bytes.Equal(planHash[:], claims.GetPlanSha256()) {
		return nil, &refusal{rejectPlanHash, "the SHA-256 of the plan bytes is not the signed plan_sha256"}
	}
	var plan routingv1.RoutePlan
	if err := proto.Unmarshal(planBytes, &plan); err != nil {
		return nil, &refusal{rejectMalformed, fmt.Sprintf("%s is not a RoutePlan: %v", HeaderRoutePlan, err)}
	}
	if plan.GetRequestId() != claims.GetRequestId() {
		return nil, &refusal{rejectRequestID, fmt.Sprintf("the plan's request_id %q is not the signed %q", plan.GetRequestId(), claims.GetRequestId())}
	}
	if int64(bodyBytes) != claims.GetBodyBytes() {
		return nil, &refusal{rejectBodyBytes, fmt.Sprintf("received %d body bytes, signed %d", bodyBytes, claims.GetBodyBytes())}
	}
	if !planListsKey(&plan) {
		return nil, &refusal{rejectEmptyPlan, "the plan lists no provider, upstream and key to dispatch to"}
	}

	if claims.GetSendId() == "" {
		return nil, &refusal{rejectMalformed, "the claims carry no send_id"}
	}
	if !p.sendIDs.claim(claims.GetSendId(), expiresAt, now) {
		return nil, &refusal{rejectSendIDReused, fmt.Sprintf("send_id %q was already used on this pod", claims.GetSendId())}
	}
	return &verifiedPlan{plan: &plan, claims: &claims}, nil
}

// planListsKey reports whether the plan names at least one key of one upstream.
func planListsKey(plan *routingv1.RoutePlan) bool {
	for _, provider := range plan.GetProviders() {
		for _, upstream := range provider.GetUpstreams() {
			if len(upstream.GetKeys()) > 0 {
				return true
			}
		}
	}
	return false
}

// rejectionBody is the JSON body of the 401 answered for a refused route plan.
type rejectionBody struct {
	Error rejectionError `json:"error"`
}

type rejectionError struct {
	Type    string    `json:"type"`
	Code    rejection `json:"code"`
	Message string    `json:"message"`
}

// rejectionType is the error type of every refused route plan.
const rejectionType = "tokun_route_plan_rejected"

func (r *refusal) body() []byte {
	data, err := json.Marshal(rejectionBody{Error: rejectionError{Type: rejectionType, Code: r.reason, Message: r.detail}})
	if err != nil {
		panic(fmt.Sprintf("tokun: encode a rejection body: %v", err))
	}
	return data
}
