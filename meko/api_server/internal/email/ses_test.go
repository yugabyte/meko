package email

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"meko/api_server/internal/service"
)

type captureSES struct{ input *sesv2.SendEmailInput }

func (c *captureSES) SendEmail(
	_ context.Context, params *sesv2.SendEmailInput, _ ...func(*sesv2.Options),
) (*sesv2.SendEmailOutput, error) {
	c.input = params
	return &sesv2.SendEmailOutput{}, nil
}

func newTestEmailer(t *testing.T, client SESAPI) *SESEmailer {
	t.Helper()
	html, err := htmlTmpl("templates/password_setup.html")
	if err != nil {
		t.Fatal(err)
	}
	text, err := textTmpl("templates/password_setup.txt")
	if err != nil {
		t.Fatal(err)
	}
	return &SESEmailer{
		client:                client,
		fromAddress:           "no-reply@example.com",
		appURL:                "https://app.example.com",
		tmplPasswordSetupHTML: html,
		tmplPasswordSetupText: text,
	}
}

func TestSendPasswordSetupEmail_IsNotTheWelcomeEmail(t *testing.T) {
	cases := []struct {
		params      service.PasswordSetupEmailParams
		wantSubject string
		want        []string
		notWant     []string
		wantIgnore  bool
	}{
		{
			params: service.PasswordSetupEmailParams{
				Code: "12345678", CodeTTL: time.Hour, Reason: service.PasswordSetupForgotPassword,
			},
			wantSubject: "Reset your Meko password",
			want:        []string{"12345678", "60 minutes", "https://app.example.com/forgot-password"},
			notWant:     []string{"temporary password"},
			wantIgnore:  true,
		},
		{
			params: service.PasswordSetupEmailParams{
				TempPassword: "Tmp-Pass-1234!ab", Reason: service.PasswordSetupInviteExpired,
			},
			wantSubject: "Your new temporary Meko password",
			want:        []string{"Tmp-Pass-1234!ab", "https://app.example.com"},
			notWant:     []string{"/forgot-password"},
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.params.Reason), func(t *testing.T) {
			ses := &captureSES{}
			p := tc.params
			p.RecipientEmail = "invited@example.com"
			if err := newTestEmailer(t, ses).SendPasswordSetupEmail(context.Background(), p); err != nil {
				t.Fatalf("send: %v", err)
			}
			msg := ses.input.Content.Simple
			if got := *msg.Subject.Data; got != tc.wantSubject {
				t.Errorf("subject = %q, want %q", got, tc.wantSubject)
			}
			for name, body := range map[string]string{"html": *msg.Body.Html.Data, "text": *msg.Body.Text.Data} {
				for _, want := range append([]string{"invited@example.com"}, tc.want...) {
					if !strings.Contains(body, want) {
						t.Errorf("%s body missing %q", name, want)
					}
				}
				for _, notWant := range tc.notWant {
					if strings.Contains(body, notWant) {
						t.Errorf("%s body must not contain %q", name, notWant)
					}
				}
				if strings.Contains(strings.ToLower(body), "welcome") {
					t.Errorf("%s body must not read as a welcome email", name)
				}
				if got := strings.Contains(body, "you can ignore this email"); got != tc.wantIgnore {
					t.Errorf("%s body ignore note = %v, want %v", name, got, tc.wantIgnore)
				}
			}
			if to := ses.input.Destination.ToAddresses; len(to) != 1 || to[0] != "invited@example.com" {
				t.Errorf("unexpected recipients %v", to)
			}
		})
	}
}

func TestSendPasswordSetupEmail_TrimsTrailingSlash(t *testing.T) {
	ses := &captureSES{}
	e := newTestEmailer(t, ses)
	e.appURL = "https://app.example.com/"
	if err := e.SendPasswordSetupEmail(context.Background(), service.PasswordSetupEmailParams{
		RecipientEmail: "invited@example.com", Code: "12345678", CodeTTL: time.Hour,
		Reason: service.PasswordSetupForgotPassword,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if body := *ses.input.Content.Simple.Body.Text.Data; strings.Contains(body, "com//forgot-password") {
		t.Errorf("link has a double slash: %s", body)
	}
}
