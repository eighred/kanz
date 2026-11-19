package gateway

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Keep business-input constraints separate from the owner's output projections.
// The gateway accepts both protobuf field names and JSON names, including mixed
// spellings in one request, but rejects two spellings of the same field.
func describeLakeInputs(schemas map[string]any) {
	selectionMessage := (&lakepb.SnapshotSelection{}).ProtoReflect().Descriptor()
	inputSchemas := make(map[string]any)
	protoSchema(selectionMessage, inputSchemas)
	selection := inputSchemas[string(selectionMessage.FullName())].(map[string]any)
	selection["description"] = "Select from 1 through 64 distinct event types. Each type must be trimmed, valid UTF-8 of 1 through 256 bytes without NUL, CR or LF. The interval includes its start and excludes its end. The owner requires the end instant to follow the start."
	selectionProps := selection["properties"].(map[string]any)
	types := selectionProps["event_types"].(map[string]any)
	types["minItems"], types["maxItems"], types["uniqueItems"] = 1, 64, true
	types["items"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 256, "pattern": `^[^\s\x00]([^\x00\r\n]*[^\s\x00])?$`}
	lakeInputNames(selection, selectionMessage, "event_types", "event_time_from", "event_time_until")
	schemas["LakeSnapshotSelectionInput"] = selection

	create := schemas["lake.v1.CreateSnapshotInput"].(map[string]any)
	createProps := create["properties"].(map[string]any)
	createProps["request_id"].(map[string]any)["pattern"] = `^[a-zA-Z0-9][a-zA-Z0-9_-]{15,127}$`
	createProps["selection"] = map[string]any{"$ref": "#/components/schemas/LakeSnapshotSelectionInput"}
	lakeInputNames(create, (&lakepb.CreateSnapshotInput{}).ProtoReflect().Descriptor(), "request_id", "selection")

	query := schemas["lake.v1.SnapshotQuery"].(map[string]any)
	props := query["properties"].(map[string]any)
	list := lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS
	count := lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE
	failures := lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_DECODE_FAILURES
	props["operation"] = map[string]any{"oneOf": []any{
		map[string]any{"type": "string", "enum": []string{list.String(), count.String(), failures.String()}},
		map[string]any{"type": "integer", "enum": []int32{int32(list), int32(count), int32(failures)}},
	}}
	props["page_size"] = map[string]any{"oneOf": []any{
		map[string]any{"type": "integer", "minimum": 1, "maximum": 1000},
		map[string]any{"type": "string", "pattern": `^([1-9][0-9]{0,2}|1000)$`},
	}}
	// JSON numbers can also carry the cursor, but JavaScript callers need the
	// decimal string form to retain the full uint64 range.
	props["after_ordinal"] = map[string]any{"oneOf": []any{
		lakeCursorString(), map[string]any{"type": "integer", "minimum": 0, "maximum": uint64(math.MaxUint64)},
	}}
	lakeInputNames(query, (&lakepb.SnapshotQuery{}).ProtoReflect().Descriptor(), "operation", "page_size")
	query["allOf"] = append(query["allOf"].([]any), map[string]any{"not": map[string]any{
		"properties": map[string]any{"operation": map[string]any{"enum": []any{count.String(), int32(count)}}},
		"anyOf":      []any{map[string]any{"required": []string{"after_ordinal"}}, map[string]any{"required": []string{"afterOrdinal"}}},
	}})
	query["description"] = "Queries require a ready snapshot. The page size is 1 through 1000. The optional ordinal cursor is exclusive. Count-by-event-type forbids a cursor and refuses a result with more groups than the page size."
}

// JSON Schema numeric bounds do not constrain strings. For a full-width
// decimal, the first digit differing from MaxUint64 determines overflow.
func lakeCursorString() map[string]any {
	maximum := strconv.FormatUint(math.MaxUint64, 10)
	var overflow []string
	for i, digit := range maximum {
		if digit < '9' {
			overflow = append(overflow, fmt.Sprintf("%s[%c-9][0-9]{%d}", maximum[:i], digit+1, len(maximum)-i-1))
		}
	}
	return map[string]any{
		"type": "string", "maxLength": len(maximum), "pattern": `^(0|[1-9][0-9]*)$`,
		"not": map[string]any{"pattern": "^(" + strings.Join(overflow, "|") + ")$"},
	}
}

func lakeInputNames(schema map[string]any, message protoreflect.MessageDescriptor, required ...string) {
	props := schema["properties"].(map[string]any)
	var names []string
	constraints, _ := schema["allOf"].([]any)
	for i := 0; i < message.Fields().Len(); i++ {
		field := message.Fields().Get(i)
		name, alias := string(field.Name()), field.JSONName()
		mandatory := slices.Contains(required, name)
		if name == alias {
			if mandatory {
				names = append(names, name)
			}
			continue
		}
		props[alias] = props[name]
		if mandatory {
			constraints = append(constraints, map[string]any{"oneOf": []any{
				map[string]any{"required": []string{name}}, map[string]any{"required": []string{alias}},
			}})
		} else {
			constraints = append(constraints, map[string]any{"not": map[string]any{"required": []string{name, alias}}})
		}
	}
	if len(names) != 0 {
		schema["required"] = names
	}
	if len(constraints) != 0 {
		schema["allOf"] = constraints
	}
}
