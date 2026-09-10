package usage

import (
	"context"
	"strings"
	"testing"
)

func TestCanonicalSchemaVersionSevenAddsReasoningAndFirstContentAt(t *testing.T) {
	definitions, err := canonicalSchemaDefinitions(context.Background(), 7)
	if err != nil {
		t.Fatalf("canonical schema version 7: %v", err)
	}
	definition, ok := definitions[schemaObjectIdentity{kind: schemaObjectTable, name: accountingRequestsTable}]
	if !ok {
		t.Fatalf("canonical schema has no %s table", accountingRequestsTable)
	}
	columns := make(map[string]schemaColumn, len(definition.columns))
	for _, column := range definition.columns {
		columns[column.name] = column
	}
	for _, name := range []string{"reasoning_output_tokens", "first_content_at"} {
		column, ok := columns[name]
		if !ok {
			t.Fatalf("canonical schema missing %s", name)
		}
		if column.dataType != "INTEGER" || column.notNull {
			t.Fatalf("canonical %s column = %#v; want nullable INTEGER", name, column)
		}
	}
	createSQL := strings.ToLower(definition.create)
	for _, clause := range []string{
		"reasoning_output_tokens integer check (reasoning_output_tokens is null or reasoning_output_tokens >= 0)",
		"first_content_at integer check (first_content_at is null or first_content_at >= upstream_dispatched_at)",
	} {
		if !strings.Contains(createSQL, clause) {
			t.Fatalf("canonical accounting_requests SQL %q does not contain %q", definition.create, clause)
		}
	}
}
