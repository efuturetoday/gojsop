package jssource

import (
	"context"
	"testing"

	corev1alpha1 "github.com/o-haase/gojsop/api/v1alpha1"
)

// js-sources.R1
func TestChain_InlineMatches(t *testing.T) {
	c := NewChain(InlineLoader{})
	body, err := c.Load(context.Background(), corev1alpha1.JSSource{Inline: "1+1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(body) != "1+1" {
		t.Fatalf("got %q", string(body))
	}
}

// js-sources.R1
func TestChain_NoSourceErrors(t *testing.T) {
	c := NewChain(InlineLoader{})
	_, err := c.Load(context.Background(), corev1alpha1.JSSource{})
	if err == nil {
		t.Fatal("expected error for empty source")
	}
}

// js-sources.R3
func TestHash_Stable(t *testing.T) {
	a := Hash([]byte("foo"))
	b := Hash([]byte("foo"))
	c := Hash([]byte("bar"))
	if a != b {
		t.Fatal("hash not stable")
	}
	if a == c {
		t.Fatal("hash should differ for different input")
	}
}
