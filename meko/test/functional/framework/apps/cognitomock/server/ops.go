package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type obj = map[string]any

var filterRe = regexp.MustCompile(`^\s*(\w+)\s*(\^=|=)\s*"(.*)"\s*$`)

func s(p obj, k string) string {
	v, _ := p[k].(string)
	return v
}

func need(p obj, k string) (string, error) {
	v, ok := p[k].(string)
	if !ok {
		return "", fail("InvalidParameterException", "Missing required parameter %s", k)
	}
	return v, nil
}

func sub(p obj, k string) obj {
	m, _ := p[k].(obj)
	if m == nil {
		return obj{}
	}
	return m
}

func attrList(v any) map[string]string {
	out := map[string]string{}
	items, _ := v.([]any)
	for _, it := range items {
		m, _ := it.(obj)
		name, _ := m["Name"].(string)
		val, _ := m["Value"].(string)
		out[name] = val
	}
	return out
}

func (p *Pool) byAccessToken(token string) (*user, error) {
	claims := p.verify(token)
	if claims == nil || claims["token_use"] != "access" {
		return nil, fail("NotAuthorizedException", "Invalid Access Token")
	}
	name, _ := claims["username"].(string)
	return p.find(name)
}

func (p *Pool) checkClient(clientID string) error {
	if clientID != p.cfg.ClientID {
		return fail("ResourceNotFoundException", "User pool client %s does not exist.", clientID)
	}
	return nil
}

func (p *Pool) checkHash(clientID, secretHash, username string) error {
	if err := p.checkClient(clientID); err != nil {
		return err
	}
	if p.cfg.ClientSecret == "" {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(p.cfg.ClientSecret))
	mac.Write([]byte(username + p.cfg.ClientID))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(secretHash)) {
		return fail("NotAuthorizedException", "Client %s is configured with secret but SECRET_HASH was not received", p.cfg.ClientID)
	}
	return nil
}

func (p *Pool) checkPool(r obj) error {
	if s(r, "UserPoolId") != p.cfg.PoolID {
		return fail("ResourceNotFoundException", "User pool does not exist.")
	}
	return nil
}

func checkPassword(pw string) error {
	var lower, upper, digit, symbol bool
	for _, c := range pw {
		switch {
		case unicode.IsLower(c):
			lower = true
		case unicode.IsUpper(c):
			upper = true
		case unicode.IsDigit(c):
			digit = true
		}
		if strings.ContainsRune(symbols, c) {
			symbol = true
		}
	}
	if len([]rune(pw)) < 8 || !lower || !upper || !digit || !symbol {
		return fail("InvalidPasswordException", "Password did not conform with policy: Password not long enough")
	}
	return nil
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func delivery(u *user, name string) obj {
	return obj{"Destination": u.Attrs["email"], "DeliveryMedium": "EMAIL", "AttributeName": name}
}

func describe(u *user) obj {
	attrs := []obj{{"Name": "sub", "Value": u.Sub}}
	names := make([]string, 0, len(u.Attrs))
	for k := range u.Attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		attrs = append(attrs, obj{"Name": k, "Value": u.Attrs[k]})
	}
	return obj{
		"Username": u.Username, "Attributes": attrs, "UserStatus": u.Status, "Enabled": true,
		"UserCreateDate": u.Created, "UserLastModifiedDate": u.Created,
	}
}

func setAttrs(u *user, items any) {
	list, _ := items.([]any)
	for _, it := range list {
		m, _ := it.(obj)
		name, _ := m["Name"].(string)
		val, _ := m["Value"].(string)
		if name == "email" && val != u.Attrs["email"] {
			u.Attrs["email_verified"] = "false"
		}
		u.Attrs[name] = val
	}
}

func (p *Pool) authResult(u *user, withRefresh bool) (obj, error) {
	access, err := p.token(u, "access", tokenOpts{})
	if err != nil {
		return nil, err
	}
	id, err := p.token(u, "id", tokenOpts{})
	if err != nil {
		return nil, err
	}
	out := obj{"AccessToken": access, "IdToken": id, "ExpiresIn": int(p.cfg.TokenTTL.Seconds()), "TokenType": "Bearer"}
	if withRefresh {
		rt := uuidHex() + uuidHex()
		p.refresh[rt] = u.Username
		out["RefreshToken"] = rt
	}
	return out, nil
}

func (p *Pool) operation(op string, r obj) (obj, error) {
	switch op {
	case "SignUp":
		return p.signUp(r)
	case "ConfirmSignUp":
		return p.confirmSignUp(r)
	case "ResendConfirmationCode":
		if err := p.checkHash(s(r, "ClientId"), s(r, "SecretHash"), s(r, "Username")); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		if u.Status != "UNCONFIRMED" {
			return nil, fail("InvalidParameterException", "User is already confirmed.")
		}
		return obj{"CodeDeliveryDetails": delivery(u, "email")}, nil
	case "InitiateAuth":
		return p.initiateAuth(r)
	case "RespondToAuthChallenge":
		return p.respondToAuthChallenge(r)
	case "GlobalSignOut":
		u, err := p.byAccessToken(s(r, "AccessToken"))
		if err != nil {
			return nil, err
		}
		for rt, name := range p.refresh {
			if name == u.Username {
				delete(p.refresh, rt)
			}
		}
		return obj{}, nil
	case "ChangePassword":
		u, err := p.byAccessToken(s(r, "AccessToken"))
		if err != nil {
			return nil, err
		}
		if !equal(u.Password, s(r, "PreviousPassword")) {
			return nil, fail("NotAuthorizedException", "Incorrect username or password.")
		}
		if err := checkPassword(s(r, "ProposedPassword")); err != nil {
			return nil, err
		}
		u.Password = s(r, "ProposedPassword")
		return obj{}, nil
	case "ForgotPassword":
		if err := p.checkHash(s(r, "ClientId"), s(r, "SecretHash"), s(r, "Username")); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		if u.Status != "CONFIRMED" {
			return nil, fail("InvalidParameterException", "Cannot reset password for the user as there is no registered/verified email or phone_number")
		}
		return obj{"CodeDeliveryDetails": delivery(u, "email")}, nil
	case "ConfirmForgotPassword":
		if err := p.checkHash(s(r, "ClientId"), s(r, "SecretHash"), s(r, "Username")); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		if err := p.checkCode(s(r, "ConfirmationCode")); err != nil {
			return nil, err
		}
		if err := checkPassword(s(r, "Password")); err != nil {
			return nil, err
		}
		u.Password = s(r, "Password")
		return obj{}, nil
	case "AdminGetUser":
		if err := p.checkPool(r); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		return describe(u), nil
	case "AdminCreateUser":
		return p.adminCreateUser(r)
	case "AdminSetUserPassword":
		if err := p.checkPool(r); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		if err := checkPassword(s(r, "Password")); err != nil {
			return nil, err
		}
		u.Password = s(r, "Password")
		u.Status = "FORCE_CHANGE_PASSWORD"
		if permanent, _ := r["Permanent"].(bool); permanent {
			u.Status = "CONFIRMED"
		}
		return obj{}, nil
	case "AdminUpdateUserAttributes":
		if err := p.checkPool(r); err != nil {
			return nil, err
		}
		u, err := p.find(s(r, "Username"))
		if err != nil {
			return nil, err
		}
		setAttrs(u, r["UserAttributes"])
		return obj{}, nil
	case "ListUsers":
		return p.listUsers(r)
	case "UpdateUserAttributes":
		u, err := p.byAccessToken(s(r, "AccessToken"))
		if err != nil {
			return nil, err
		}
		before := u.Attrs["email"]
		setAttrs(u, r["UserAttributes"])
		if u.Attrs["email"] == before {
			return obj{"CodeDeliveryDetailsList": []any{}}, nil
		}
		return obj{"CodeDeliveryDetailsList": []obj{delivery(u, "email")}}, nil
	case "GetUserAttributeVerificationCode":
		u, err := p.byAccessToken(s(r, "AccessToken"))
		if err != nil {
			return nil, err
		}
		name := s(r, "AttributeName")
		if name == "" {
			name = "email"
		}
		return obj{"CodeDeliveryDetails": delivery(u, name)}, nil
	case "VerifyUserAttribute":
		u, err := p.byAccessToken(s(r, "AccessToken"))
		if err != nil {
			return nil, err
		}
		if err := p.checkCode(s(r, "Code")); err != nil {
			return nil, err
		}
		name := s(r, "AttributeName")
		if name == "" {
			name = "email"
		}
		u.Attrs[name+"_verified"] = "true"
		return obj{}, nil
	}
	return nil, fail("InvalidActionException", "%s is not implemented", op)
}

func (p *Pool) checkCode(code string) error {
	if code != p.cfg.ConfirmationCode {
		return fail("CodeMismatchException", "Invalid verification code provided, please try again.")
	}
	return nil
}

func (p *Pool) signUp(r obj) (obj, error) {
	username, err := need(r, "Username")
	if err != nil {
		return nil, err
	}
	if err := p.checkHash(s(r, "ClientId"), s(r, "SecretHash"), username); err != nil {
		return nil, err
	}
	if err := checkPassword(s(r, "Password")); err != nil {
		return nil, err
	}
	attrs := attrList(r["UserAttributes"])
	attrs["email_verified"] = "false"
	u, err := p.newUser(username, s(r, "Password"), attrs, "UNCONFIRMED")
	if err != nil {
		return nil, err
	}
	return obj{"UserConfirmed": false, "UserSub": u.Sub, "CodeDeliveryDetails": delivery(u, "email")}, nil
}

func (p *Pool) confirmSignUp(r obj) (obj, error) {
	username, err := need(r, "Username")
	if err != nil {
		return nil, err
	}
	if err := p.checkHash(s(r, "ClientId"), s(r, "SecretHash"), username); err != nil {
		return nil, err
	}
	u, err := p.find(username)
	if err != nil {
		return nil, err
	}
	if u.Status != "UNCONFIRMED" {
		return nil, fail("NotAuthorizedException", "User cannot be confirmed. Current status is %s", u.Status)
	}
	if err := p.checkCode(s(r, "ConfirmationCode")); err != nil {
		return nil, err
	}
	u.Status = "CONFIRMED"
	u.Attrs["email_verified"] = "true"
	return obj{}, nil
}

func (p *Pool) initiateAuth(r obj) (obj, error) {
	flow, params := s(r, "AuthFlow"), sub(r, "AuthParameters")
	if flow == "REFRESH_TOKEN_AUTH" {
		if err := p.checkClient(s(r, "ClientId")); err != nil {
			return nil, err
		}
		name, ok := p.refresh[s(params, "REFRESH_TOKEN")]
		if !ok {
			return nil, fail("NotAuthorizedException", "Invalid Refresh Token")
		}
		u, err := p.find(name)
		if err != nil {
			return nil, err
		}
		res, err := p.authResult(u, false)
		return obj{"AuthenticationResult": res}, err
	}
	if flow != "USER_PASSWORD_AUTH" {
		return nil, fail("InvalidParameterException", "Unsupported auth flow %s", flow)
	}
	username := s(params, "USERNAME")
	if err := p.checkHash(s(r, "ClientId"), s(params, "SECRET_HASH"), username); err != nil {
		return nil, err
	}
	u := p.users[key(username)]
	if u == nil || !equal(u.Password, s(params, "PASSWORD")) {
		return nil, fail("NotAuthorizedException", "Incorrect username or password.")
	}
	switch u.Status {
	case "UNCONFIRMED":
		return nil, fail("UserNotConfirmedException", "User is not confirmed.")
	case "FORCE_CHANGE_PASSWORD":
		session := uuidHex()
		p.sessions[session] = u.Username
		return obj{
			"ChallengeName": "NEW_PASSWORD_REQUIRED", "Session": session,
			"ChallengeParameters": obj{"USER_ID_FOR_SRP": u.Username},
		}, nil
	}
	res, err := p.authResult(u, true)
	return obj{"AuthenticationResult": res}, err
}

func (p *Pool) respondToAuthChallenge(r obj) (obj, error) {
	if s(r, "ChallengeName") != "NEW_PASSWORD_REQUIRED" {
		return nil, fail("InvalidParameterException", "Unsupported challenge")
	}
	resp := sub(r, "ChallengeResponses")
	if err := p.checkHash(s(r, "ClientId"), s(resp, "SECRET_HASH"), s(resp, "USERNAME")); err != nil {
		return nil, err
	}
	session := s(r, "Session")
	name, ok := p.sessions[session]
	if !ok || strings.ToLower(name) != strings.ToLower(s(resp, "USERNAME")) {
		return nil, fail("NotAuthorizedException", "Invalid session for the user.")
	}
	u, err := p.find(name)
	if err != nil {
		return nil, err
	}
	if err := checkPassword(s(resp, "NEW_PASSWORD")); err != nil {
		return nil, err
	}
	u.Password = s(resp, "NEW_PASSWORD")
	u.Status = "CONFIRMED"
	delete(p.sessions, session)
	for k, v := range resp {
		if attr, ok := strings.CutPrefix(k, "userAttributes."); ok {
			u.Attrs[attr], _ = v.(string)
		}
	}
	res, err := p.authResult(u, true)
	return obj{"AuthenticationResult": res}, err
}

func (p *Pool) adminCreateUser(r obj) (obj, error) {
	if err := p.checkPool(r); err != nil {
		return nil, err
	}
	username, err := need(r, "Username")
	if err != nil {
		return nil, err
	}
	if s(r, "MessageAction") == "RESEND" {
		u, err := p.find(username)
		if err != nil {
			return nil, err
		}
		u.Password, u.Status = tempPassword(), "FORCE_CHANGE_PASSWORD"
		return obj{"User": describe(u)}, nil
	}
	password := s(r, "TemporaryPassword")
	if password == "" {
		password = tempPassword()
	}
	u, err := p.newUser(username, password, attrList(r["UserAttributes"]), "FORCE_CHANGE_PASSWORD")
	if err != nil {
		return nil, err
	}
	return obj{"User": describe(u)}, nil
}

func (p *Pool) listUsers(r obj) (obj, error) {
	if err := p.checkPool(r); err != nil {
		return nil, err
	}
	users := p.ordered()
	if flt := s(r, "Filter"); flt != "" {
		m := filterRe.FindStringSubmatch(flt)
		if m == nil {
			return nil, fail("InvalidParameterException", "Error while parsing filter")
		}
		name, op, want := m[1], m[2], m[3]
		value := func(u *user) string {
			switch name {
			case "sub":
				return u.Sub
			case "username":
				return u.Username
			}
			return u.Attrs[name]
		}
		kept := users[:0]
		for _, u := range users {
			if (op == "=" && value(u) == want) || (op == "^=" && strings.HasPrefix(value(u), want)) {
				kept = append(kept, u)
			}
		}
		users = kept
	}
	limit := 60
	if n, ok := r["Limit"].(float64); ok && n > 0 {
		limit = int(n)
	}
	out := []obj{}
	for _, u := range users {
		if len(out) >= limit {
			break
		}
		out = append(out, describe(u))
	}
	return obj{"Users": out}, nil
}
