package pg

import (
	"errors"
	"testing"
)

func TestWrapTransportCarriesTheAddr(t *testing.T) {
	err := wrapTransport(errors.New("unexpected EOF"), "127.0.0.1:5001")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("wrapTransport did not mark %v as a transport error", err)
	}
	if te.Addr != "127.0.0.1:5001" {
		t.Fatalf("Addr = %q, want the connection's address", te.Addr)
	}
	if !errors.Is(err, te.Err) {
		t.Fatal("the original error must stay unwrappable")
	}
}

func TestWrapTransportLeavesNonTransportErrorsAlone(t *testing.T) {
	for _, in := range []error{
		errors.New("connection refused"),
		errors.New("ERROR: duplicate key (SQLSTATE 23505)"),
	} {
		var te *TransportError
		if errors.As(wrapTransport(in, "127.0.0.1:5001"), &te) {
			t.Errorf("wrapTransport(%v) marked a non-transport error", in)
		}
	}
	if wrapTransport(nil, "127.0.0.1:5001") != nil {
		t.Error("wrapTransport(nil) must stay nil")
	}
}
