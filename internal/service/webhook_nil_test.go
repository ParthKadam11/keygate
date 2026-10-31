package service

import (
	"context"
	"testing"
)

// No webhook service is a service that sends nothing, as no mailer is.
func TestNilWebhookServiceSendsNothing(t *testing.T) {
	var s *WebhookService
	s.Dispatch(context.Background(), "p", "license.created", nil)
	if err := s.DispatchWithLog(context.Background(), "p", "license.created", nil); err != nil {
		t.Fatalf("DispatchWithLog on no service = %v, want nil", err)
	}
}
