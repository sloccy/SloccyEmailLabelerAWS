package db

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// fakeSSM is an in-memory ssmAPI double for the account-token side-channel.
type fakeSSM struct {
	params map[string]string
}

func newFakeSSM() *fakeSSM { return &fakeSSM{params: map[string]string{}} }

func (f *fakeSSM) GetParameter(_ context.Context, in *ssm.GetParameterInput, _ ...func(*ssm.Options)) (*ssm.GetParameterOutput, error) {
	v, ok := f.params[*in.Name]
	if !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	return &ssm.GetParameterOutput{Parameter: &ssmtypes.Parameter{Value: aws.String(v)}}, nil
}

func (f *fakeSSM) PutParameter(_ context.Context, in *ssm.PutParameterInput, _ ...func(*ssm.Options)) (*ssm.PutParameterOutput, error) {
	f.params[*in.Name] = *in.Value
	return &ssm.PutParameterOutput{}, nil
}

func (f *fakeSSM) DeleteParameter(_ context.Context, in *ssm.DeleteParameterInput, _ ...func(*ssm.Options)) (*ssm.DeleteParameterOutput, error) {
	if _, ok := f.params[*in.Name]; !ok {
		return nil, &ssmtypes.ParameterNotFound{}
	}
	delete(f.params, *in.Name)
	return &ssm.DeleteParameterOutput{}, nil
}

func TestAccountTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := &Store{ssm: newFakeSSM()}

	// Missing token reads back as empty, not an error (placeholder accounts).
	got, err := s.getAccountToken(ctx, 7)
	if err != nil || got != "" {
		t.Fatalf("getAccountToken(missing) = %q, %v; want \"\", nil", got, err)
	}

	if err := s.putAccountToken(ctx, 7, `{"refresh_token":"r"}`); err != nil {
		t.Fatalf("putAccountToken: %v", err)
	}
	got, err = s.getAccountToken(ctx, 7)
	if err != nil || got != `{"refresh_token":"r"}` {
		t.Fatalf("getAccountToken = %q, %v", got, err)
	}

	// hydrate fills CredentialsJSON from SSM when the item carried none.
	a := Account{ID: 7}
	if err := s.hydrateAccountToken(ctx, &a); err != nil {
		t.Fatalf("hydrateAccountToken: %v", err)
	}
	if a.CredentialsJSON != `{"refresh_token":"r"}` {
		t.Fatalf("hydrated CredentialsJSON = %q", a.CredentialsJSON)
	}

	// Delete is idempotent: second delete of the same param is not an error.
	if err := s.deleteAccountToken(ctx, 7); err != nil {
		t.Fatalf("deleteAccountToken: %v", err)
	}
	if err := s.deleteAccountToken(ctx, 7); err != nil {
		t.Fatalf("deleteAccountToken(again): %v", err)
	}
	if got, _ := s.getAccountToken(ctx, 7); got != "" {
		t.Fatalf("token survived delete: %q", got)
	}
}

func TestTokenParamName(t *testing.T) {
	if got := tokenParamName(42); got != "/ollamail/accounts/42/token" {
		t.Fatalf("tokenParamName(42) = %q", got)
	}
}
