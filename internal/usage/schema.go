package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// canonicalSchemaSource is where the canonical schema is built. It is private to
// one connection and never reaches a filesystem, so building it can neither
// touch the source under inspection nor leave anything behind.
const canonicalSchemaSource = ":memory:"

const finalizedResponseAccountingMigrationVersion = 2

const rateCardsAndFinalCostMigrationVersion = 3

const importedLedgerProvenanceMigrationVersion = 4

const upstreamTargetDispatchEvidenceMigrationVersion = 5

const summaryQueryIndexesMigrationVersion = 6

const (
	summaryWindowIndexName        = "accounting_requests_summary_window"
	summaryContextWindowIndexName = "accounting_requests_summary_context_window"
)

const (
	schemaMigrationsTable   = "schema_migrations"
	accountingRequestsTable = "accounting_requests"
	rateCardsTable          = "rate_cards"
)

// schemaObjectKind is the value sqlite_master records in its `type` column for an
// object the migrations create.
type schemaObjectKind string

const (
	schemaObjectTable   schemaObjectKind = "table"
	schemaObjectIndex   schemaObjectKind = "index"
	schemaObjectTrigger schemaObjectKind = "trigger"
)

// schemaObjectIdentity is what identifies one object in a database: its kind
// together with its name. A name alone does not, because SQLite keeps a separate
// namespace per kind — a trigger may hold the exact name of an index, and both
// live in the file at once. Keyed by name alone, the two collapse into one entry
// and whichever `sqlite_master` row is read last decides what the other kind is
// taken to be, which is a verdict about row order rather than about the schema.
type schemaObjectIdentity struct {
	kind schemaObjectKind
	name string
}

// compareSchemaObjectIdentity orders identities by name first so a refusal names
// the same object it always did when several are wrong, with the kind breaking
// the tie two namespaces can produce.
func compareSchemaObjectIdentity(left, right schemaObjectIdentity) int {
	if byName := strings.Compare(left.name, right.name); byName != 0 {
		return byName
	}
	return strings.Compare(string(left.kind), string(right.kind))
}

// schemaDefinition is everything a database says about one object it holds: what
// sqlite_master records for it, the columns SQLite parses out of a table's
// definition, and the shape it gives an index. Built from this build's own
// migrations it is the definition a database recording that version must hold —
// a claim `PRAGMA integrity_check` cannot judge, since a table rebuilt without
// its constraints is perfectly coherent, and one no later migration repairs,
// since a recorded migration is never replayed. The kind is not repeated here:
// it is half of the schemaObjectIdentity the definition is filed under.
type schemaDefinition struct {
	// table is sqlite_master's tbl_name: the object itself for a table, the table
	// it is attached to for an index or trigger.
	table string
	// create is the stored CREATE statement, normalized. It is the only evidence
	// there is about a trigger's body or a table's constraints.
	create  string
	columns []schemaColumn
	index   schemaIndexShape
}

// schemaColumn is one column as SQLite parsed it, in declaration order. Names
// alone would accept a column that kept its name and lost the type, NOT NULL,
// default or primary key its migration gave it.
type schemaColumn struct {
	name     string
	dataType string
	notNull  bool
	dflt     sql.NullString
	primary  int
	hidden   int
}

// schemaIndexShape is what an index does rather than what it is called: whether
// it enforces uniqueness, whether it covers only part of the table, and the keys
// it is ordered by.
type schemaIndexShape struct {
	unique  bool
	partial bool
	origin  string
	keys    []schemaIndexKey
}

type schemaIndexKey struct {
	name      sql.NullString
	descend   bool
	collation string
}

type migration struct {
	version    int
	name       string
	statements []string
}

func declaredMigrations() []migration {
	return []migration{
		accountingRequestsMigration(),
		finalizedResponseAccountingMigration(),
		rateCardsAndFinalCostMigration(),
		importedLedgerProvenanceMigration(),
		upstreamTargetDispatchEvidenceMigration(),
		summaryQueryIndexesMigration(),
	}
}

// canonicalSchemaDefinitions builds, in a throwaway in-memory database, the
// schema this build's migrations produce at version, and reads back what SQLite
// made of them. That is the definition every source claiming this version is held
// to.
//
// The schema is built rather than transcribed because it cannot be transcribed
// honestly: `accounting_requests` is created once and extended by five later
// migrations, and the CREATE text SQLite stores for it afterwards is whatever
// those ALTERs appended. A hand-maintained copy would drift, and a drifting
// canon either waves through the divergence it exists to catch or refuses good
// databases. Nothing here touches the source — the source is never written, and
// this database exists only in memory.
func canonicalSchemaDefinitions(
	ctx context.Context,
	version int,
) (definitions map[schemaObjectIdentity]schemaDefinition, result error) {
	declared := declaredMigrations()
	through := 0
	for _, candidate := range declared {
		if candidate.version > version {
			break
		}
		through++
	}
	if through == 0 || declared[through-1].version != version {
		return nil, fmt.Errorf("schema version %d is not one this build declares", version)
	}
	db, openErr := sql.Open("sqlite", canonicalSchemaSource)
	if openErr != nil {
		return nil, fmt.Errorf("build canonical schema version %d: %w", version, openErr)
	}
	// An in-memory database belongs to the connection that created it. A pool free
	// to open a second connection would migrate a second, empty database and then
	// read one of the two at random.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close canonical schema version %d: %w", version, closeErr))
		}
	}()
	if migrateErr := applyMigrations(ctx, db, declared[:through]); migrateErr != nil {
		return nil, fmt.Errorf("build canonical schema version %d: %w", version, migrateErr)
	}
	built, readErr := readSchemaDefinitions(ctx, db)
	if readErr != nil {
		return nil, fmt.Errorf("build canonical schema version %d: %w", version, readErr)
	}
	return built, nil
}

// validateRecordedSchemaObjects proves a database still means what the migrations
// it records made it mean. Nothing else establishes that. A valid prefix of
// migration rows says only that those statements once ran; `PRAGMA
// integrity_check` reports `ok` over a table dropped outright, an index rebuilt
// without UNIQUE, or a trigger whose body has become `SELECT 1`; and a migration
// already recorded is skipped rather than replayed, so no later step puts the
// real definition back. Without this, a database whose rate-card immutability
// triggers have been gutted reads as a supported schema, and the snapshot taken
// from it certifies the tampering as a good backup.
//
// Holding the declared objects to their definitions is only half of it, because
// a schema can be tampered with by addition as readily as by subtraction, and an
// addition is active rather than inert: a trigger runs on every write to the
// table it is attached to, and an index decides which writes that table accepts
// at all. So the objects attached to the tables those migrations create are also
// held to the list — see checkUndeclaredManagedSchemaObjects. Away from those
// tables the contract stays a floor: an unrelated table or view, and whatever is
// attached to it, is neither required nor refused.
func validateRecordedSchemaObjects(ctx context.Context, queryer schemaQueryer, version int) error {
	canonical, canonicalErr := canonicalSchemaDefinitions(ctx, version)
	if canonicalErr != nil {
		return fmt.Errorf("not a patchbay usage database: %w", canonicalErr)
	}
	present, presentErr := readSchemaDefinitions(ctx, queryer)
	if presentErr != nil {
		return fmt.Errorf("not a patchbay usage database: %w", presentErr)
	}
	required := slices.Collect(maps.Keys(canonical))
	slices.SortFunc(required, compareSchemaObjectIdentity)
	for _, identity := range required {
		requiredErr := checkRecordedSchemaObject(version, identity, canonical[identity], present)
		if requiredErr != nil {
			return requiredErr
		}
	}
	attachments, attachmentErr := readSchemaAttachments(ctx, queryer)
	if attachmentErr != nil {
		return fmt.Errorf("not a patchbay usage database: %w", attachmentErr)
	}
	return checkUndeclaredManagedSchemaObjects(version, canonical, attachments)
}

// managedSchemaTables is the set of tables the recorded version's migrations
// create. Everything attached to one of them is Patchbay's to declare: these
// hold the accounting rows, the rate cards, and the migration bookkeeping, and
// nothing outside the migrations has business changing how they are written.
func managedSchemaTables(canonical map[schemaObjectIdentity]schemaDefinition) map[string]struct{} {
	managed := make(map[string]struct{}, len(canonical))
	for identity := range canonical {
		if identity.kind == schemaObjectTable {
			managed[identity.name] = struct{}{}
		}
	}
	return managed
}

// checkUndeclaredManagedSchemaObjects refuses a trigger or an index attached to a
// managed table that the recorded version's migrations never created. Proving
// every declared object still matches its definition says nothing about what has
// been added beside it, and nothing else downstream would: the extra object makes
// every canonical definition compare equal, reports `ok` to
// `PRAGMA integrity_check`, is copied forward intact by `VACUUM INTO`, and
// survives the forward-migration proof, because migrations create objects and
// never remove one they do not know about.
//
// A trigger is refused outright. Its body is arbitrary SQL that runs on every
// write to the table it is attached to, so an `AFTER INSERT ... DELETE FROM
// accounting_requests` on an otherwise exact database is a ledger that empties
// itself the moment Patchbay records a request — and a snapshot taken from it
// certifies that as a good backup.
//
// An index is refused on the same table for the same reason at one remove: it
// decides which writes are accepted. A UNIQUE one rejects rows the migrations
// accept, whether it spans the whole table or, as a partial index, only the rows
// its `WHERE` selects. A nonunique one is not the safe residue it looks like —
// a key over an expression or a collation the writing build cannot evaluate
// fails every insert into the table it covers — so all of them are refused
// rather than a subset whose safety would have to be re-argued per index.
//
// The one exception is the index SQLite builds for itself to enforce a UNIQUE or
// PRIMARY KEY constraint, which `rate_cards` requires and which no migration
// names. SQLite stores no CREATE text for those, and that absence is what
// identifies them: reserving the `sqlite_` name prefix instead would exempt the
// namespace an attacker reaches by writing `sqlite_master` directly, which is
// where a planted trigger would then sit unexamined.
//
// What earns an attachment its exemption is its full identity, kind included,
// standing on the table the migrations attached it to. A name is not enough:
// SQLite files a trigger and an index of one name in separate namespaces and
// keeps both, so a trigger wearing the name of a canonical index is a second
// object rather than that index — and exempting it by name would wave through
// the `DELETE FROM accounting_requests` this sweep exists to stop, with every
// declared object still present and exact.
func checkUndeclaredManagedSchemaObjects(
	version int,
	canonical map[schemaObjectIdentity]schemaDefinition,
	attachments []schemaAttachment,
) error {
	managed := managedSchemaTables(canonical)
	for _, attachment := range attachments {
		identity := schemaObjectIdentity{kind: attachment.kind, name: attachment.name}
		if declared, isDeclared := canonical[identity]; isDeclared && declared.table == attachment.table {
			continue
		}
		if _, isManaged := managed[attachment.table]; !isManaged {
			continue
		}
		if undeclaredErr := undeclaredManagedSchemaObjectError(version, attachment); undeclaredErr != nil {
			return undeclaredErr
		}
	}
	return nil
}

// undeclaredManagedSchemaObjectError names why an addition to a managed table is
// refused, and reports nil for the implicit index SQLite maintains for its own
// constraints.
func undeclaredManagedSchemaObjectError(version int, attachment schemaAttachment) error {
	switch attachment.kind {
	case schemaObjectTrigger:
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d does not declare trigger %q on table %q, "+
				"and an undeclared trigger runs on every write to the table it is attached to",
			version, attachment.name, attachment.table,
		)
	case schemaObjectIndex:
		if attachment.implicit {
			return nil
		}
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d does not declare index %q on table %q, "+
				"and an undeclared index changes which writes that table accepts",
			version, attachment.name, attachment.table,
		)
	default:
		return nil
	}
}

// schemaAttachment is one object sqlite_master hangs off a table, reduced to what
// judging its legitimacy needs.
type schemaAttachment struct {
	kind  schemaObjectKind
	name  string
	table string
	// implicit marks an object SQLite created rather than a statement did. It has
	// no stored CREATE text, which is the only durable evidence of the
	// distinction — the reserved `sqlite_` name prefix is not, since a principal
	// who can write the file can insert a row claiming any name at all.
	implicit bool
}

// readSchemaAttachments reads every trigger and index the database holds,
// deliberately without the reserved-name exclusion the definition passes apply.
// `CREATE` refuses a `sqlite_` name, but writing the row directly does not, and
// SQLite honors the trigger it finds there — so the one namespace a check may
// not skip is the one that looks like SQLite's own.
func readSchemaAttachments(
	ctx context.Context,
	queryer schemaQueryer,
) (attachments []schemaAttachment, result error) {
	const query = `SELECT source.type, source.name, source.tbl_name, source.sql IS NULL
		FROM sqlite_master AS source
		WHERE source.type IN ('index', 'trigger')
		ORDER BY source.name`
	rows, queryErr := queryer.QueryContext(ctx, query)
	if queryErr != nil {
		return nil, fmt.Errorf("read attached schema objects: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close attached schema object rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var attachment schemaAttachment
		var kind string
		var implicit int
		if scanErr := rows.Scan(&kind, &attachment.name, &attachment.table, &implicit); scanErr != nil {
			return nil, fmt.Errorf("scan attached schema objects: %w", scanErr)
		}
		attachment.kind = schemaObjectKind(kind)
		attachment.implicit = implicit != 0
		attachments = append(attachments, attachment)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate attached schema objects: %w", rowsErr)
	}
	return attachments, nil
}

// checkRecordedSchemaObject holds one source object to its canonical definition,
// narrowing as it goes so the refusal names the smallest thing that is wrong. The
// structural comparisons run before the CREATE text: both would catch a column
// that changed type, and the column-level refusal says which column.
//
// The lookup is by full identity, so an object of another kind wearing the
// required name never stands in for it. When the required identity is absent the
// other namespaces are searched for that name anyway — not to accept it, but so
// a database whose index has been replaced by a table is told that, rather than
// that its index has simply gone missing.
func checkRecordedSchemaObject(
	version int,
	identity schemaObjectIdentity,
	canonical schemaDefinition,
	present map[schemaObjectIdentity]schemaDefinition,
) error {
	found, exists := present[identity]
	if !exists {
		if impostor, taken := schemaObjectKindHoldingName(present, identity.name); taken {
			return fmt.Errorf(
				"not a patchbay usage database: schema version %d requires %s %q, but it is a %s",
				version, identity.kind, identity.name, impostor,
			)
		}
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d requires %s %q, which is missing",
			version, identity.kind, identity.name,
		)
	}
	if found.table != canonical.table {
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d requires %s %q on table %q, but it is on %q",
			version, identity.kind, identity.name, canonical.table, found.table,
		)
	}
	columnErr := checkRecordedSchemaColumns(version, identity.name, canonical.columns, found.columns)
	if columnErr != nil {
		return columnErr
	}
	if identity.kind == schemaObjectIndex && !sameSchemaIndexShape(canonical.index, found.index) {
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d requires index %q to match its migration "+
				"definition, but this database builds it differently",
			version, identity.name,
		)
	}
	if found.create != canonical.create {
		return fmt.Errorf(
			"not a patchbay usage database: schema version %d requires %s %q to match its migration "+
				"definition, but this database defines it differently",
			version, identity.kind, identity.name,
		)
	}
	return nil
}

// schemaObjectKindHoldingName reports the kind of an object holding name, for
// the refusal that has already established no object of the required kind does.
// Several kinds can hold one name at once, so the lowest in identity order is
// named and the sweep accounts for the rest.
func schemaObjectKindHoldingName(
	present map[schemaObjectIdentity]schemaDefinition,
	name string,
) (schemaObjectKind, bool) {
	var holder schemaObjectKind
	found := false
	for identity := range present {
		if identity.name != name {
			continue
		}
		if !found || identity.kind < holder {
			holder = identity.kind
			found = true
		}
	}
	return holder, found
}

// checkRecordedSchemaColumns reports a column that is gone before one that is
// merely wrong, since a dropped column is what a reader of the refusal can act on
// most directly. Position is compared alongside the rest: a table rebuilt with
// its columns reordered holds the same names for different data.
func checkRecordedSchemaColumns(version int, table string, canonical, present []schemaColumn) error {
	for _, required := range canonical {
		if !slices.ContainsFunc(present, func(candidate schemaColumn) bool { return candidate.name == required.name }) {
			return fmt.Errorf(
				"not a patchbay usage database: schema version %d requires column %q on table %q, which is missing",
				version, required.name, table,
			)
		}
	}
	for position, required := range canonical {
		if position >= len(present) || present[position] != required {
			return fmt.Errorf(
				"not a patchbay usage database: schema version %d requires column %q on table %q to match its "+
					"migration definition, but this database declares it differently",
				version, required.name, table,
			)
		}
	}
	return nil
}

func sameSchemaIndexShape(canonical, present schemaIndexShape) bool {
	return canonical.unique == present.unique &&
		canonical.partial == present.partial &&
		canonical.origin == present.origin &&
		slices.Equal(canonical.keys, present.keys)
}

// readSchemaDefinitions reads every object a database holds, along with the
// structure SQLite parsed out of each one. Objects SQLite creates for itself
// carry a reserved `sqlite_` name and are left out: they are no migration's
// promise, and the implicit index behind a UNIQUE constraint would be compared
// against a name that varies with the order constraints were declared in.
//
// Every pass uses the table-valued form of its PRAGMA, taking each object name
// from sqlite_master inside the query, so no identifier is spliced into
// statement text.
func readSchemaDefinitions(
	ctx context.Context,
	queryer schemaQueryer,
) (map[schemaObjectIdentity]schemaDefinition, error) {
	definitions, objectErr := readSchemaObjects(ctx, queryer)
	if objectErr != nil {
		return nil, objectErr
	}
	if columnErr := readSchemaColumns(ctx, queryer, definitions); columnErr != nil {
		return nil, columnErr
	}
	if indexErr := readSchemaIndexShapes(ctx, queryer, definitions); indexErr != nil {
		return nil, indexErr
	}
	if keyErr := readSchemaIndexKeys(ctx, queryer, definitions); keyErr != nil {
		return nil, keyErr
	}
	return definitions, nil
}

// schemaObjectScope excludes SQLite's own objects from every schema pass.
const schemaObjectScope = ` AND source.name NOT LIKE 'sqlite\_%' ESCAPE '\'`

// readSchemaObjects files each row under its full identity and refuses a schema
// that holds two rows under one. Collecting them into a map keeps whichever
// arrived last, so a gutted definition planted beside the real one would win or
// lose on row order — there is no honest verdict to give about such a schema, and
// giving one anyway is what makes it a bypass rather than an oddity.
func readSchemaObjects(
	ctx context.Context,
	queryer schemaQueryer,
) (definitions map[schemaObjectIdentity]schemaDefinition, result error) {
	const query = `SELECT source.type, source.name, source.tbl_name, IFNULL(source.sql, '')
		FROM sqlite_master AS source
		WHERE 1 = 1` + schemaObjectScope
	rows, queryErr := queryer.QueryContext(ctx, query)
	if queryErr != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close sqlite_master rows: %w", closeErr))
		}
	}()
	definitions = map[schemaObjectIdentity]schemaDefinition{}
	for rows.Next() {
		var kind, name, table, create string
		if scanErr := rows.Scan(&kind, &name, &table, &create); scanErr != nil {
			return nil, fmt.Errorf("scan sqlite_master: %w", scanErr)
		}
		identity := schemaObjectIdentity{kind: schemaObjectKind(kind), name: name}
		if _, duplicate := definitions[identity]; duplicate {
			return nil, fmt.Errorf(
				"sqlite_master holds more than one %s named %q", identity.kind, identity.name,
			)
		}
		definitions[identity] = schemaDefinition{
			table:  table,
			create: normalizeSchemaSQL(create),
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, fmt.Errorf("iterate sqlite_master: %w", rowsErr)
	}
	return definitions, nil
}

func readSchemaColumns(
	ctx context.Context,
	queryer schemaQueryer,
	definitions map[schemaObjectIdentity]schemaDefinition,
) (result error) {
	const query = `SELECT source.name, info.name, info.type, info."notnull", info.dflt_value, info.pk, info.hidden
		FROM sqlite_master AS source
		JOIN pragma_table_xinfo(source.name) AS info
		WHERE source.type = 'table'` + schemaObjectScope + `
		ORDER BY source.name, info.cid`
	rows, queryErr := queryer.QueryContext(ctx, query)
	if queryErr != nil {
		return fmt.Errorf("read table columns: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close table column rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var table string
		var column schemaColumn
		var notNull int
		if scanErr := rows.Scan(
			&table, &column.name, &column.dataType, &notNull, &column.dflt, &column.primary, &column.hidden,
		); scanErr != nil {
			return fmt.Errorf("scan table columns: %w", scanErr)
		}
		column.notNull = notNull != 0
		identity := schemaObjectIdentity{kind: schemaObjectTable, name: table}
		definition, known := definitions[identity]
		if !known {
			continue
		}
		definition.columns = append(definition.columns, column)
		definitions[identity] = definition
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate table columns: %w", rowsErr)
	}
	return nil
}

func readSchemaIndexShapes(
	ctx context.Context,
	queryer schemaQueryer,
	definitions map[schemaObjectIdentity]schemaDefinition,
) (result error) {
	const query = `SELECT source.name, list."unique", list.partial, list.origin
		FROM sqlite_master AS source
		JOIN pragma_index_list(source.tbl_name) AS list ON list.name = source.name
		WHERE source.type = 'index'` + schemaObjectScope
	rows, queryErr := queryer.QueryContext(ctx, query)
	if queryErr != nil {
		return fmt.Errorf("read index shapes: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close index shape rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var name string
		var unique, partial int
		var origin string
		if scanErr := rows.Scan(&name, &unique, &partial, &origin); scanErr != nil {
			return fmt.Errorf("scan index shapes: %w", scanErr)
		}
		identity := schemaObjectIdentity{kind: schemaObjectIndex, name: name}
		definition, known := definitions[identity]
		if !known {
			continue
		}
		definition.index.unique = unique != 0
		definition.index.partial = partial != 0
		definition.index.origin = origin
		definitions[identity] = definition
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate index shapes: %w", rowsErr)
	}
	return nil
}

// readSchemaIndexKeys reads the keys an index is ordered by, in order. The
// trailing entries `PRAGMA index_xinfo` reports for the row identifier it appends
// are not keys and say nothing about what the index promises.
func readSchemaIndexKeys(
	ctx context.Context,
	queryer schemaQueryer,
	definitions map[schemaObjectIdentity]schemaDefinition,
) (result error) {
	const query = `SELECT source.name, info.name, info."desc", IFNULL(info.coll, '')
		FROM sqlite_master AS source
		JOIN pragma_index_xinfo(source.name) AS info
		WHERE source.type = 'index' AND info.key = 1` + schemaObjectScope + `
		ORDER BY source.name, info.seqno`
	rows, queryErr := queryer.QueryContext(ctx, query)
	if queryErr != nil {
		return fmt.Errorf("read index keys: %w", queryErr)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close index key rows: %w", closeErr))
		}
	}()
	for rows.Next() {
		var index string
		var key schemaIndexKey
		var descend int
		if scanErr := rows.Scan(&index, &key.name, &descend, &key.collation); scanErr != nil {
			return fmt.Errorf("scan index keys: %w", scanErr)
		}
		key.descend = descend != 0
		identity := schemaObjectIdentity{kind: schemaObjectIndex, name: index}
		definition, known := definitions[identity]
		if !known {
			continue
		}
		definition.index.keys = append(definition.index.keys, key)
		definitions[identity] = definition
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("iterate index keys: %w", rowsErr)
	}
	return nil
}

// normalizeSchemaSQL reduces a stored CREATE statement to the form two databases
// must agree on: comments dropped, runs of whitespace collapsed to one space, and
// everything outside a quoted region lowercased. SQLite is indifferent to all
// three — keywords and unquoted identifiers are case-insensitive — so this
// absorbs formatting and nothing else. Quoted regions are copied through byte for
// byte, which is what keeps a trigger raising 'rate cards are immutable' distinct
// from one raising anything else.
//
// That is the compatibility contract for databases written by earlier builds.
// Migrations are immutable, so the text SQLite stored for an object came from the
// same statements this build declares, and the ALTERs that extend a table append
// the same column definitions in the same order; formatting is the only drift the
// comparison has to survive. Two definitions that differ in meaning cannot
// normalize alike, so what this absorbs can only ever be presentation.
func normalizeSchemaSQL(text string) string {
	var normalized strings.Builder
	normalized.Grow(len(text))
	pending := false
	for index := 0; index < len(text); {
		remainder := text[index:]
		switch {
		case isSchemaSQLSpace(text[index]):
			index++
			pending = normalized.Len() > 0
		case strings.HasPrefix(remainder, "--"):
			index += schemaSQLCommentLength(remainder, "\n")
			pending = normalized.Len() > 0
		case strings.HasPrefix(remainder, "/*"):
			index += schemaSQLCommentLength(remainder, "*/")
			pending = normalized.Len() > 0
		default:
			if pending {
				normalized.WriteByte(' ')
				pending = false
			}
			if quoted := schemaSQLQuotedLength(remainder); quoted > 0 {
				normalized.WriteString(remainder[:quoted])
				index += quoted
				continue
			}
			normalized.WriteByte(lowerSchemaSQLByte(text[index]))
			index++
		}
	}
	return normalized.String()
}

// schemaSQLQuotedLength measures the quoted region starting at text, or reports 0
// when none does. An unterminated region runs to the end of the statement rather
// than being guessed at: copying it through verbatim can only refuse a database,
// never accept one.
func schemaSQLQuotedLength(text string) int {
	var closing byte
	switch text[0] {
	case '\'', '"', '`':
		closing = text[0]
	case '[':
		closing = ']'
	default:
		return 0
	}
	for index := 1; index < len(text); index++ {
		if text[index] != closing {
			continue
		}
		// A doubled delimiter is an escaped one. Bracketed identifiers have no
		// escape: SQLite ends one at the first `]`.
		if closing != ']' && index+1 < len(text) && text[index+1] == closing {
			index++
			continue
		}
		return index + 1
	}
	return len(text)
}

// schemaSQLCommentLength measures a comment through its terminator, or to the end
// of the statement when it has none — which is where SQLite itself ends one.
func schemaSQLCommentLength(text, terminator string) int {
	end := strings.Index(text[2:], terminator)
	if end < 0 {
		return len(text)
	}
	return 2 + end + len(terminator)
}

func isSchemaSQLSpace(character byte) bool {
	switch character {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

// lowerSchemaSQLByte folds ASCII only, which is exactly how far SQLite's own
// case-insensitivity reaches.
func lowerSchemaSQLByte(character byte) byte {
	if character >= 'A' && character <= 'Z' {
		return character + ('a' - 'A')
	}
	return character
}

func accountingRequestsMigration() migration {
	return migration{
		version: 1,
		name:    "accounting_requests",
		statements: []string{
			`CREATE TABLE accounting_requests (
				id INTEGER PRIMARY KEY,
				started_at INTEGER NOT NULL,
				completed_at INTEGER,
				upstream_dispatched_at INTEGER,
				request_kind TEXT NOT NULL,
				lifecycle_state TEXT NOT NULL,
				outcome TEXT,
				upstream_contacted INTEGER NOT NULL,
				route_host TEXT NOT NULL,
				route_context TEXT NOT NULL,
				route_alias TEXT,
				route_upstream TEXT NOT NULL,
				request_model TEXT NOT NULL,
				resolved_model TEXT NOT NULL,
				auth_mode TEXT NOT NULL,
				credential_class TEXT NOT NULL,
				billing_policy TEXT NOT NULL,
				billing_class TEXT,
				cost_status TEXT NOT NULL
			)`,
		},
	}
}

func finalizedResponseAccountingMigration() migration {
	return migration{
		version: finalizedResponseAccountingMigrationVersion,
		name:    "finalized_response_accounting",
		statements: []string{
			`ALTER TABLE accounting_requests ADD COLUMN response_id TEXT`,
			`ALTER TABLE accounting_requests ADD COLUMN http_status INTEGER CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599)`,
			`ALTER TABLE accounting_requests ADD COLUMN normalization_complete INTEGER CHECK (normalization_complete IS NULL OR normalization_complete IN (0, 1))`,
			`ALTER TABLE accounting_requests ADD COLUMN input_tokens INTEGER CHECK (input_tokens IS NULL OR input_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN output_tokens INTEGER CHECK (output_tokens IS NULL OR output_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN cache_creation_input_tokens INTEGER CHECK (cache_creation_input_tokens IS NULL OR cache_creation_input_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN cache_read_input_tokens INTEGER CHECK (cache_read_input_tokens IS NULL OR cache_read_input_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN cache_creation_5m_input_tokens INTEGER CHECK (cache_creation_5m_input_tokens IS NULL OR cache_creation_5m_input_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN cache_creation_1h_input_tokens INTEGER CHECK (cache_creation_1h_input_tokens IS NULL OR cache_creation_1h_input_tokens >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN provider_cost_present INTEGER CHECK (provider_cost_present IS NULL OR provider_cost_present IN (0, 1))`,
			`ALTER TABLE accounting_requests ADD COLUMN provider_cost_valid INTEGER CHECK (provider_cost_valid IS NULL OR provider_cost_valid IN (0, 1))`,
			`ALTER TABLE accounting_requests ADD COLUMN provider_cost_nano_usd INTEGER CHECK (provider_cost_nano_usd IS NULL OR provider_cost_nano_usd >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN stop_reason TEXT`,
			`ALTER TABLE accounting_requests ADD COLUMN block_shape TEXT`,
			`ALTER TABLE accounting_requests ADD COLUMN duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN upstream_ms INTEGER CHECK (upstream_ms IS NULL OR upstream_ms >= 0)`,
			`CREATE TRIGGER accounting_requests_validate_finalized_snapshot
			BEFORE UPDATE ON accounting_requests
			WHEN NEW.lifecycle_state = 'finalized' AND (
				NEW.completed_at IS NULL
				OR NEW.completed_at < NEW.started_at
				OR NEW.outcome IS NULL
				OR NEW.outcome NOT IN ('success', 'local_rejection', 'upstream_http_failure', 'transport_failure', 'response_interrupted', 'normalization_failure')
				OR NEW.cost_status NOT IN ('known', 'unknown')
				OR NEW.normalization_complete IS NULL
				OR NEW.provider_cost_present IS NULL
				OR NEW.provider_cost_valid IS NULL
				OR NEW.provider_cost_nano_usd IS NULL
				OR NOT (
					(NEW.provider_cost_present = 0 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 1 AND NEW.provider_cost_nano_usd >= 0)
				)
				OR NEW.response_id = ''
				OR NEW.stop_reason IS NULL
				OR NEW.block_shape IS NULL
				OR NEW.duration_ms IS NULL
				OR NEW.upstream_ms IS NULL
				OR NEW.upstream_contacted NOT IN (0, 1)
				OR (NEW.upstream_contacted = 0 AND NEW.upstream_dispatched_at IS NOT NULL)
				OR (NEW.upstream_contacted = 1 AND NEW.upstream_dispatched_at IS NULL)
				OR (NEW.upstream_dispatched_at IS NOT NULL AND NEW.completed_at < NEW.upstream_dispatched_at)
			)
			BEGIN
				SELECT RAISE(ABORT, 'invalid finalized accounting snapshot');
			END`,
			`CREATE UNIQUE INDEX accounting_requests_finalized_response_id ON accounting_requests(response_id) WHERE lifecycle_state = 'finalized' AND response_id IS NOT NULL AND response_id <> ''`,
		},
	}
}

func importedLedgerProvenanceMigration() migration {
	return migration{
		version: importedLedgerProvenanceMigrationVersion,
		name:    "imported_ledger_provenance",
		statements: []string{
			`ALTER TABLE accounting_requests ADD COLUMN import_source TEXT`,
			`ALTER TABLE accounting_requests ADD COLUMN import_line INTEGER CHECK (
				(import_line IS NULL OR import_line > 0)
				AND (import_source IS NULL) = (import_line IS NULL)
			)`,
			`CREATE UNIQUE INDEX accounting_requests_import_provenance
			ON accounting_requests(import_source, import_line) WHERE import_source IS NOT NULL`,
			`CREATE TRIGGER accounting_requests_validate_finalized_snapshot_insert
			BEFORE INSERT ON accounting_requests
			WHEN NEW.lifecycle_state = 'finalized' AND (
				NEW.completed_at IS NULL
				OR NEW.completed_at < NEW.started_at
				OR NEW.outcome IS NULL
				OR NEW.outcome NOT IN ('success', 'local_rejection', 'upstream_http_failure', 'transport_failure', 'response_interrupted', 'normalization_failure')
				OR NEW.cost_status NOT IN ('known', 'unknown')
				OR NEW.normalization_complete IS NULL
				OR NEW.provider_cost_present IS NULL
				OR NEW.provider_cost_valid IS NULL
				OR NEW.provider_cost_nano_usd IS NULL
				OR NOT (
					(NEW.provider_cost_present = 0 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 0 AND NEW.provider_cost_nano_usd = 0)
					OR (NEW.provider_cost_present = 1 AND NEW.provider_cost_valid = 1 AND NEW.provider_cost_nano_usd >= 0)
				)
				OR NEW.response_id = ''
				OR NEW.stop_reason IS NULL
				OR NEW.block_shape IS NULL
				OR NEW.duration_ms IS NULL
				OR NEW.upstream_ms IS NULL
				OR NEW.upstream_contacted NOT IN (0, 1)
				OR (NEW.upstream_contacted = 0 AND NEW.upstream_dispatched_at IS NOT NULL)
				OR (NEW.upstream_contacted = 1 AND NEW.upstream_dispatched_at IS NULL)
				OR (NEW.upstream_dispatched_at IS NOT NULL AND NEW.completed_at < NEW.upstream_dispatched_at)
			)
			BEGIN
				SELECT RAISE(ABORT, 'invalid finalized accounting snapshot');
			END`,
		},
	}
}

func upstreamTargetDispatchEvidenceMigration() migration {
	return migration{
		version: upstreamTargetDispatchEvidenceMigrationVersion,
		name:    "upstream_target_dispatch_evidence",
		statements: []string{
			`ALTER TABLE accounting_requests ADD COLUMN upstream_target TEXT CHECK (
				upstream_target IS NULL OR (length(upstream_target) > 0 AND upstream_contacted = 1)
			)`,
		},
	}
}

func summaryQueryIndexesMigration() migration {
	return migration{
		version: summaryQueryIndexesMigrationVersion,
		name:    "summary_query_indexes",
		statements: []string{
			`CREATE INDEX ` + summaryWindowIndexName + `
			ON accounting_requests(started_at, request_kind)`,
			`CREATE INDEX ` + summaryContextWindowIndexName + `
			ON accounting_requests(route_context, started_at, request_kind)`,
		},
	}
}

func rateCardsAndFinalCostMigration() migration {
	return migration{
		version: rateCardsAndFinalCostMigrationVersion,
		name:    "rate_cards_and_final_cost",
		statements: []string{
			`CREATE TABLE rate_cards (
				id INTEGER PRIMARY KEY,
				model TEXT NOT NULL,
				effective_from INTEGER NOT NULL,
				input_nano_usd_per_million INTEGER NOT NULL CHECK (input_nano_usd_per_million >= 0),
				output_nano_usd_per_million INTEGER NOT NULL CHECK (output_nano_usd_per_million >= 0),
				cache_read_nano_usd_per_million INTEGER NOT NULL CHECK (cache_read_nano_usd_per_million >= 0),
				cache_creation_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_nano_usd_per_million >= 0),
				cache_creation_5m_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_5m_nano_usd_per_million >= 0),
				cache_creation_1h_nano_usd_per_million INTEGER NOT NULL CHECK (cache_creation_1h_nano_usd_per_million >= 0),
				source TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				UNIQUE(model, effective_from)
			)`,
			`CREATE TRIGGER rate_cards_forbid_update
			BEFORE UPDATE ON rate_cards
			BEGIN
				SELECT RAISE(ABORT, 'rate cards are immutable');
			END`,
			`CREATE TRIGGER rate_cards_forbid_delete
			BEFORE DELETE ON rate_cards
			BEGIN
				SELECT RAISE(ABORT, 'rate cards are immutable');
			END`,
			`ALTER TABLE accounting_requests ADD COLUMN cost_basis TEXT CHECK (cost_basis IS NULL OR cost_basis IN ('provider_reported', 'rate_card', 'zero_marginal', 'unknown'))`,
			`ALTER TABLE accounting_requests ADD COLUMN final_cost_nano_usd INTEGER CHECK (final_cost_nano_usd IS NULL OR final_cost_nano_usd >= 0)`,
			`ALTER TABLE accounting_requests ADD COLUMN rate_card_id INTEGER CHECK (rate_card_id IS NULL OR rate_card_id > 0) REFERENCES rate_cards(id)`,
		},
	}
}

func migrate(ctx context.Context, db *sql.DB) error {
	return applyMigrations(ctx, db, declaredMigrations())
}

func validateMigrations(declared []migration) error {
	previous := 0
	for _, candidate := range declared {
		if candidate.version <= previous {
			return fmt.Errorf(
				"migration version %d (%q) is duplicate or out of order after %d",
				candidate.version,
				candidate.name,
				previous,
			)
		}
		if strings.TrimSpace(candidate.name) == "" {
			return fmt.Errorf("migration version %d has a blank name", candidate.version)
		}
		previous = candidate.version
	}
	return nil
}

func applyMigrations(ctx context.Context, db *sql.DB, declared []migration) error {
	if err := validateMigrations(declared); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (unixepoch('subsec') * 1000000000)
	)`); err != nil {
		return fmt.Errorf("bootstrap schema migrations: %w", err)
	}
	if err := rejectFutureVersion(ctx, db, declared); err != nil {
		return err
	}
	for _, candidate := range declared {
		if err := applyMigration(ctx, db, candidate); err != nil {
			return err
		}
	}
	return nil
}

func rejectFutureVersion(ctx context.Context, db *sql.DB, declared []migration) error {
	latest := 0
	if len(declared) > 0 {
		latest = declared[len(declared)-1].version
	}
	var future int
	err := db.QueryRowContext(
		ctx,
		`SELECT version FROM schema_migrations WHERE version > ? ORDER BY version DESC LIMIT 1`,
		latest,
	).Scan(&future)
	if err == nil {
		return fmt.Errorf(
			"unknown future schema version %d (latest supported is %d)",
			future,
			latest,
		)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect schema version: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, candidate migration) (result error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration %d: %w", candidate.version, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			result = errors.Join(result, fmt.Errorf("close migration connection: %w", closeErr))
		}
	}()

	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration %d (%s): %w", candidate.version, candidate.name, err)
	}
	if err = executeMigration(ctx, conn, candidate); err != nil {
		return rollbackMigration(ctx, conn, err)
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return rollbackMigration(
			ctx,
			conn,
			fmt.Errorf("commit migration %d (%s): %w", candidate.version, candidate.name, err),
		)
	}
	return nil
}

func executeMigration(ctx context.Context, conn *sql.Conn, candidate migration) error {
	var recordedName string
	err := conn.QueryRowContext(
		ctx,
		`SELECT name FROM schema_migrations WHERE version = ?`,
		candidate.version,
	).Scan(&recordedName)
	if err == nil {
		if recordedName != candidate.name {
			return fmt.Errorf(
				"schema version %d is recorded as %q, want %q",
				candidate.version,
				recordedName,
				candidate.name,
			)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect migration %d: %w", candidate.version, err)
	}

	for _, statement := range candidate.statements {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", candidate.version, candidate.name, err)
		}
	}
	if _, err = conn.ExecContext(
		ctx,
		`INSERT INTO schema_migrations(version, name) VALUES (?, ?)`,
		candidate.version,
		candidate.name,
	); err != nil {
		return fmt.Errorf("record migration %d (%s): %w", candidate.version, candidate.name, err)
	}
	return nil
}

func rollbackMigration(ctx context.Context, conn *sql.Conn, cause error) error {
	if _, rollbackErr := conn.ExecContext(ctx, `ROLLBACK`); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("roll back migration: %w", rollbackErr))
	}
	return cause
}
