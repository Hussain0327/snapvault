package search

import (
	"reflect"
	"testing"
)

func TestTermsLowercasesAndSplitsOnNonLetterDigit(t *testing.T) {
	got := Terms("Payment Systems, 2024!", false)
	want := []string{"payment", "systems", "2024"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Terms(...) = %v, want %v", got, want)
	}
}

func TestTermsKeepsStopwordsWhenNotRequested(t *testing.T) {
	got := Terms("the payment is for the merchant", false)
	want := []string{"the", "payment", "is", "for", "the", "merchant"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Terms(..., false) = %v, want %v", got, want)
	}
}

func TestTermsDropsStopwordsWhenRequested(t *testing.T) {
	got := Terms("the payment is for the merchant", true)
	want := []string{"payment", "merchant"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Terms(..., true) = %v, want %v", got, want)
	}
}

func TestTermsEmptyText(t *testing.T) {
	if got := Terms("", true); got != nil {
		t.Errorf("Terms(\"\", true) = %v, want nil", got)
	}
}

func TestTermsAllStopwords(t *testing.T) {
	got := Terms("the is on", true)
	if len(got) != 0 {
		t.Errorf("Terms(\"the is on\", true) = %v, want empty", got)
	}
}
