// Package util holds small shared helpers: caller identity, model families,
// usage extraction and prompt estimation.
package util

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"github.com/Schallophy/literouter/internal/model"
)

var (
	bearerPattern  = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)
	readableLabel  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,24}$`)
)

// IdentifyCaller derives an opaque, stable caller identity from Authorization.
func IdentifyCaller(headers http.Header) model.CallerIdentity {
	return IdentifyCallerValue(headers.Get("authorization"))
}

// IdentifyCallerValue is IdentifyCaller for a raw authorization header.
func IdentifyCallerValue(authorization string) model.CallerIdentity {
	match := bearerPattern.FindStringSubmatch(strings.TrimSpace(authorization))
	token := ""
	if match != nil {
		token = strings.TrimSpace(match[1])
	}
	if token == "" {
		return model.AnonymousCaller
	}
	sum := sha256.Sum256([]byte(token))
	id := hex.EncodeToString(sum[:])[:16]
	return model.CallerIdentity{ID: id, Label: LabelFor(token)}
}

// LabelFor turns short readable tokens such as sk-opencode into "opencode"
// and masks anything longer so raw keys never reach state.json.
func LabelFor(token string) string {
	body := token
	if strings.HasPrefix(token, "sk-") {
		body = token[3:]
	}
	if readableLabel.MatchString(body) {
		return body
	}
	runes := []rune(token)
	if len(runes) <= 10 {
		return string(runes)
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-4:])
}
