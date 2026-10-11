package mail

import (
	"bytes"
	"fmt"
	"html/template"

	"github.com/ai-code-101/rag-backend/api/internal/models"
)

var htmlTpl = template.Must(template.New("otp").Parse(`<!doctype html>
<html><body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="padding:32px 0;"><tr><td align="center">
<table role="presentation" width="480" cellspacing="0" cellpadding="0" style="max-width:480px;width:100%;background:#ffffff;border-radius:12px;overflow:hidden;">
<tr><td style="background:#4f46e5;padding:20px 28px;color:#ffffff;font-size:20px;font-weight:700;">{{.AppName}}</td></tr>
<tr><td style="padding:28px;color:#111827;font-size:15px;line-height:1.6;">
<p style="margin:0 0 12px;">{{.Intro}}</p>
<p style="margin:24px 0;text-align:center;"><span style="display:inline-block;font-size:32px;letter-spacing:8px;font-weight:700;background:#eef2ff;color:#3730a3;padding:12px 20px;border-radius:8px;">{{.Code}}</span></p>
<p style="margin:0 0 12px;color:#4b5563;">This code expires in {{.Minutes}} minutes. Never share it with anyone.</p>
<p style="margin:0;color:#6b7280;font-size:13px;">If you didn't request this, you can safely ignore this email.</p>
</td></tr>
<tr><td style="padding:16px 28px;background:#f9fafb;color:#9ca3af;font-size:12px;">&copy; {{.AppName}}</td></tr>
</table></td></tr></table></body></html>`))

type otpView struct {
	AppName string
	Intro   string
	Code    string
	Minutes int
}

// OTPEmail renders the verification / login / reset email for the given purpose.
func OTPEmail(appName, to string, purpose models.OTPPurpose, code string, minutes int) (Message, error) {
	var subject, intro string
	switch purpose {
	case models.PurposeVerifyEmail:
		subject = fmt.Sprintf("Verify your %s email", appName)
		intro = "Use this code to verify your email address:"
	case models.PurposeLogin:
		subject = fmt.Sprintf("Your %s login code", appName)
		intro = "Use this code to sign in:"
	case models.PurposeResetPassword:
		subject = fmt.Sprintf("Reset your %s password", appName)
		intro = "Use this code to reset your password:"
	default:
		return Message{}, fmt.Errorf("unknown purpose %q", purpose)
	}
	var html bytes.Buffer
	if err := htmlTpl.Execute(&html, otpView{AppName: appName, Intro: intro, Code: code, Minutes: minutes}); err != nil {
		return Message{}, err
	}
	text := fmt.Sprintf("%s\n\n%s\n\n%s\n\nThis code expires in %d minutes. Never share it with anyone.\nIf you didn't request this, you can ignore this email.\n",
		appName, intro, code, minutes)
	return Message{To: to, Subject: subject, Text: text, HTML: html.String()}, nil
}
