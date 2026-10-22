package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
)

// The web UI's sign-in. meko ui asks for a code with the admin token and opens the UI with it in
// the link; the page swaps it for the client token. The link passes through the browser's command
// line, which other local users can read, so it carries a code that works once and briefly rather
// than the token itself.
const (
	uiCodeRoute   = "/api/v1/ui-code"
	uiRedeemRoute = "/api/v1/ui-code/redeem"
	// UICodeTTL is how long a code works: long enough for a browser to start.
	UICodeTTL = 60 * time.Second
)

type uiCodes struct {
	mu    sync.Mutex
	codes map[string]time.Time // code to its expiry
}

func (u *uiCodes) issue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b)
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	for c, exp := range u.codes {
		if now.After(exp) {
			delete(u.codes, c)
		}
	}
	u.codes[code] = now.Add(UICodeTTL)
	return code, nil
}

// take spends a code: true once, for a code issued and not yet expired.
func (u *uiCodes) take(code string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	exp, ok := u.codes[code]
	delete(u.codes, code)
	return ok && time.Now().Before(exp)
}

// UICodeReply is POST /api/v1/ui-code.
type UICodeReply struct {
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

func (s *Server) issueUICode(c echo.Context) error {
	code, err := s.codes.issue()
	if err != nil {
		return err
	}
	return envelope.OK(c, UICodeReply{Code: code, ExpiresIn: int(UICodeTTL.Seconds())})
}

// UIRedeemReply is POST /api/v1/ui-code/redeem: the client token ("" with auth = "none").
type UIRedeemReply struct {
	Token string `json:"token"`
}

func (s *Server) redeemUICode(c echo.Context) error {
	var body struct {
		Code string `json:"code"`
	}
	if err := c.Bind(&body); err != nil || body.Code == "" || !s.codes.take(body.Code) {
		return envelope.Fail(c, http.StatusUnauthorized, "INVALID_CODE",
			"This link has expired or was used already. Run meko ui again.")
	}
	return envelope.OK(c, UIRedeemReply{Token: s.opts.ClientToken})
}
