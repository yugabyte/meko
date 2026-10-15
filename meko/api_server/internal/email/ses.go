package email

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	texttemplate "text/template"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"meko/api_server/internal/service"
)

//go:embed templates
var templateFS embed.FS

func htmlTmpl(path string) (*template.Template, error) {
	b, err := templateFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("email: missing template %s: %w", path, err)
	}
	return template.New(path).Parse(string(b))
}

func textTmpl(path string) (*texttemplate.Template, error) {
	b, err := templateFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("email: missing template %s: %w", path, err)
	}
	return texttemplate.New(path).Parse(string(b))
}

// SESAPI is the subset of the SES v2 client used by SESEmailer.
type SESAPI interface {
	SendEmail(ctx context.Context, params *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SESEmailer sends datapack invite and password-setup emails via AWS SES.
type SESEmailer struct {
	client                SESAPI
	fromAddress           string
	appURL                string
	tmplExistingHTML      *template.Template
	tmplExistingText      *texttemplate.Template
	tmplNewUserHTML       *template.Template
	tmplNewUserText       *texttemplate.Template
	tmplPasswordSetupHTML *template.Template
	tmplPasswordSetupText *texttemplate.Template
}

// SESConfig holds the configuration for creating a SESEmailer.
type SESConfig struct {
	Region      string
	FromAddress string
	AppURL      string
	// AccessKey and SecretKey are optional. When empty, the ambient AWS
	// credential chain (IRSA, instance profile, environment) is used.
	AccessKey string
	SecretKey string
}

// NewSESEmailer creates a SESEmailer. Returns an error if Region or
// FromAddress are empty.
func NewSESEmailer(ctx context.Context, cfg SESConfig) (*SESEmailer, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("ses: region is required")
	}
	if cfg.FromAddress == "" {
		return nil, fmt.Errorf("ses: from_address is required")
	}

	var awsOpts []func(*awsconfig.LoadOptions) error
	awsOpts = append(awsOpts, awsconfig.WithRegion(cfg.Region))
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		awsOpts = append(awsOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsOpts...)
	if err != nil {
		return nil, fmt.Errorf("ses: load aws config: %w", err)
	}

	tmplExistingHTML, err := htmlTmpl("templates/invite_existing.html")
	if err != nil {
		return nil, err
	}
	tmplExistingText, err := textTmpl("templates/invite_existing.txt")
	if err != nil {
		return nil, err
	}
	tmplNewUserHTML, err := htmlTmpl("templates/invite_new_user.html")
	if err != nil {
		return nil, err
	}
	tmplNewUserText, err := textTmpl("templates/invite_new_user.txt")
	if err != nil {
		return nil, err
	}
	tmplPasswordSetupHTML, err := htmlTmpl("templates/password_setup.html")
	if err != nil {
		return nil, err
	}
	tmplPasswordSetupText, err := textTmpl("templates/password_setup.txt")
	if err != nil {
		return nil, err
	}

	return &SESEmailer{
		client:                sesv2.NewFromConfig(awsCfg),
		fromAddress:           cfg.FromAddress,
		appURL:                cfg.AppURL,
		tmplExistingHTML:      tmplExistingHTML,
		tmplExistingText:      tmplExistingText,
		tmplNewUserHTML:       tmplNewUserHTML,
		tmplNewUserText:       tmplNewUserText,
		tmplPasswordSetupHTML: tmplPasswordSetupHTML,
		tmplPasswordSetupText: tmplPasswordSetupText,
	}, nil
}

// SendInviteEmail satisfies service.EmailSender. It picks the appropriate
// template based on whether the recipient is a new or existing Meko user.
func (s *SESEmailer) SendInviteEmail(ctx context.Context, p service.InviteEmailParams) error {
	if p.AppURL == "" {
		p.AppURL = s.appURL
	}

	var subject, htmlBody, textBody string

	var tmplHTML *template.Template
	var tmplText *texttemplate.Template

	if p.IsExistingUser {
		subject = fmt.Sprintf(`%s shared "%s" with you on Meko`, p.OwnerName, p.DatapackName)
		tmplHTML, tmplText = s.tmplExistingHTML, s.tmplExistingText
	} else {
		subject = fmt.Sprintf(`You've been invited to Meko — "%s" is waiting for you`, p.DatapackName)
		tmplHTML, tmplText = s.tmplNewUserHTML, s.tmplNewUserText
	}

	var err error
	if htmlBody, err = renderHTML(tmplHTML, p); err != nil {
		return fmt.Errorf("ses: render html template: %w", err)
	}
	if textBody, err = renderText(tmplText, p); err != nil {
		return fmt.Errorf("ses: render text template: %w", err)
	}

	charset := "UTF-8"
	input := &sesv2.SendEmailInput{
		FromEmailAddress: &s.fromAddress,
		Destination: &sestypes.Destination{
			ToAddresses: []string{p.RecipientEmail},
		},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: &subject, Charset: &charset},
				Body: &sestypes.Body{
					Html: &sestypes.Content{Data: &htmlBody, Charset: &charset},
					Text: &sestypes.Content{Data: &textBody, Charset: &charset},
				},
			},
		},
	}

	_, err = s.client.SendEmail(ctx, input)
	if err != nil {
		slog.Error("ses: send email failed",
			"to", p.RecipientEmail,
			"datapack", p.DatapackName,
			"new_user", !p.IsExistingUser,
			"error", err,
		)
		return fmt.Errorf("ses: send email: %w", err)
	}
	slog.Info("ses: invite email sent",
		"to", p.RecipientEmail,
		"datapack", p.DatapackName,
		"new_user", !p.IsExistingUser,
	)
	return nil
}

// passwordSetupView is the data the password-setup templates render. The
// wording that depends on the reason is picked here, so the templates stay
// free of branching beyond which secret to show.
type passwordSetupView struct {
	RecipientEmail string
	Code           string
	TempPassword   string
	AppURL         string
	Heading        string
	Intro          string
	IgnoreNote     bool
}

func newPasswordSetupView(p service.PasswordSetupEmailParams, appURL string) (subject string, v passwordSetupView) {
	if p.AppURL != "" {
		appURL = p.AppURL
	}
	v = passwordSetupView{
		RecipientEmail: p.RecipientEmail,
		AppURL:         strings.TrimRight(appURL, "/"),
	}
	if p.Reason == service.PasswordSetupInviteExpired {
		v.TempPassword = p.TempPassword
		v.Heading = "Your new temporary Meko password"
		v.Intro = "The temporary password from your Meko invite expired. " +
			"Sign in with this new temporary password, then choose your own password."
		return v.Heading, v
	}
	v.Code = p.Code
	v.Heading = "Reset your Meko password"
	v.Intro = fmt.Sprintf("You asked to reset your Meko password. Enter this code on the Forgot password page, "+
		"then choose a new password. The code expires in %d minutes and works once.", int(p.CodeTTL.Minutes()))
	v.IgnoreNote = true
	return v.Heading, v
}

// SendPasswordSetupEmail satisfies service.PasswordSetupEmailSender. It sends
// a reset code or a fresh temporary password to a user who never finished
// first sign-in, using its own template rather than the invite's welcome
// email.
func (s *SESEmailer) SendPasswordSetupEmail(ctx context.Context, p service.PasswordSetupEmailParams) error {
	subject, view := newPasswordSetupView(p, s.appURL)
	htmlBody, err := renderHTML(s.tmplPasswordSetupHTML, view)
	if err != nil {
		return fmt.Errorf("ses: render password-setup html template: %w", err)
	}
	textBody, err := renderText(s.tmplPasswordSetupText, view)
	if err != nil {
		return fmt.Errorf("ses: render password-setup text template: %w", err)
	}

	charset := "UTF-8"
	_, err = s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: &s.fromAddress,
		Destination: &sestypes.Destination{
			ToAddresses: []string{p.RecipientEmail},
		},
		Content: &sestypes.EmailContent{
			Simple: &sestypes.Message{
				Subject: &sestypes.Content{Data: &subject, Charset: &charset},
				Body: &sestypes.Body{
					Html: &sestypes.Content{Data: &htmlBody, Charset: &charset},
					Text: &sestypes.Content{Data: &textBody, Charset: &charset},
				},
			},
		},
	})
	if err != nil {
		slog.Error("ses: send password-setup email failed",
			"to", p.RecipientEmail, "reason", string(p.Reason), "error", err)
		return fmt.Errorf("ses: send email: %w", err)
	}
	slog.Info("ses: password-setup email sent", "to", p.RecipientEmail, "reason", string(p.Reason))
	return nil
}

func renderHTML(tmpl *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderText(tmpl *texttemplate.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
