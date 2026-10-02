package live

import (
	"reflect"
	"testing"
)

func TestPurchaseParsingIgnoresMentions(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []PurchaseItem
	}{
		{text: "@pessoa2026 boa"},
		{text: "@pessoa.1374 boa"},
		{text: "@1374 @pessoa_1485 @quero"},
		{text: "@pessoa.2 @pessoa2026 boa noite"},
		{text: "@pessoa2026 quero 1374 x2", want: []PurchaseItem{{Keyword: "1374", Quantity: 2}}},
		{text: "1374 @pessoa.25", want: []PurchaseItem{{Keyword: "1374", Quantity: 1}}},
		{text: "@valor @cancela Código1485 X2", want: []PurchaseItem{{Keyword: "1485", Quantity: 2}}},
		{text: "@pessoa2026 1374 x2, 1485 × 3", want: []PurchaseItem{
			{Keyword: "1374", Quantity: 2}, {Keyword: "1485", Quantity: 3},
		}},
		{text: "quero 2 @pessoa2026", want: []PurchaseItem{{Quantity: 2}}},
		{text: "@pessoa2026 não quero 1374"},
		{text: "@pessoa2026 quanto custa 1374"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			if got := ParsePurchaseItems(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("items=%+v want=%+v", got, tc.want)
			}
			intent := ParsePurchaseIntent(tc.text)
			if tc.want == nil {
				if intent != nil {
					t.Fatalf("mention became purchase: %+v", intent)
				}
				return
			}
			quantity := 0
			for _, item := range tc.want {
				quantity += item.Quantity
			}
			if intent == nil || intent.Quantity != quantity || intent.RawText != tc.text {
				t.Fatalf("intent lost quantity or original text: %+v", intent)
			}
		})
	}
}

func TestKeywordExtractionIgnoresMentions(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{text: "@pessoa.1374 @1485"},
		{text: "@pessoa.1374 quero 1485 x2", want: []string{"1485"}},
		{text: "1374, @pessoa.1374 1485", want: []string{"1374", "1485"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got := ExtractPossibleKeywords(tc.text)
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("keywords=%v want=%v", got, tc.want)
			}
		})
	}
}
