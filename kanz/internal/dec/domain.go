package dec

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// REFUSING A MESSAGE IS CHEAPER THAN AUDITING ITS FIELDS (#95).
//
// FromProtoChecked bounds ONE conversion. That is the right tool where a
// decoder has an error path per field, and the wrong one where it does not:
// tv-sync builds its order DTO through helpers returning bare strings, so a
// per-field refusal would have to be threaded through five call sites that
// have nowhere to put it — and the next Decimal field added to the schema
// would arrive unchecked anyway, silently, because nothing would fail.
//
// InDomainDeep asks the question once, of the whole message, at the point it
// comes off the wire. A consumer that has just unmarshalled a payload holds a
// CONCRETE generated type, so protoreflect can walk it with no schema registry
// and no event_type→message mapping — which is what makes this usable at every
// consumer rather than only where a type switch already exists.

// decimalName is the message this walk is looking for. Matching by descriptor
// name (rather than a Go type assertion) means a dynamicpb message decoded from
// a registry is checked identically to a generated one.
const decimalName protoreflect.FullName = "common.v1.Decimal"

// maxWalkDepth stops a pathological nesting from exhausting the stack. Real
// payloads nest a handful of levels; exceeding this is refused rather than
// ignored, because a message this decoder cannot fully inspect is exactly the
// message it must not pass on.
const maxWalkDepth = 64

// InDomainDeep reports whether every common.v1.Decimal anywhere inside m — at any
// nesting depth, including inside repeated and map fields — is in domain.
//
// It returns the field PATH of the first failure ("fill.fee.amount"), because a
// DLQ entry saying "an exponent was out of domain" sends an operator reading a
// binary payload by hand; one naming the field does not.
//
// A nil message is in domain: absent is not out of range.
func InDomainDeep(m proto.Message) (string, bool) {
	if m == nil {
		return "", true
	}
	return walkDomain(m.ProtoReflect(), "", 0)
}

func walkDomain(m protoreflect.Message, path string, depth int) (string, bool) {
	if !m.IsValid() {
		return "", true // a nil/unset message has no Decimals to be out of domain
	}
	if depth > maxWalkDepth {
		return path, false
	}
	if m.Descriptor().FullName() == decimalName {
		return path, exponentInDomain(m)
	}

	badPath, ok := "", true
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		var p string
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() != protoreflect.MessageKind {
				return true
			}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if p, ok = walkDomain(mv.Message(), fmt.Sprintf("%s[%v]", child(path, fd), k), depth+1); !ok {
					badPath = p
					return false
				}
				return true
			})
		case fd.IsList():
			if fd.Kind() != protoreflect.MessageKind {
				return true
			}
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				if p, ok = walkDomain(l.Get(i).Message(), fmt.Sprintf("%s[%d]", child(path, fd), i), depth+1); !ok {
					badPath = p
					break
				}
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			if p, ok = walkDomain(v.Message(), child(path, fd), depth+1); !ok {
				badPath = p
			}
		}
		return ok // stop the Range at the first failure
	})
	return badPath, ok
}

// exponentInDomain applies the same bound as InDomain, reached reflectively.
// A Decimal whose descriptor has no exponent field is refused rather than
// assumed fine: it means this walk no longer understands the type it is
// checking, and guessing is how a bound stops being enforced without anything
// failing.
func exponentInDomain(m protoreflect.Message) bool {
	fd := m.Descriptor().Fields().ByName("exponent")
	if fd == nil {
		return false
	}
	exp := m.Get(fd).Int()
	return exp >= -maxSafeExponent && exp <= maxSafeExponent
}

func child(path string, fd protoreflect.FieldDescriptor) string {
	if path == "" {
		return string(fd.Name())
	}
	return path + "." + string(fd.Name())
}
