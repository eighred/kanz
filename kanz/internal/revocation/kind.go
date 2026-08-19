package revocation

// FeedKind is the discriminator every feed body carries, and the reader refuses
// a body without it.
//
// WITHOUT IT, A MISDIRECTED URL IS AN EMPTY DENYLIST THAT LOOKS HEALTHY. Every
// field of Feed is optional to a JSON decoder, so ANY 200 carrying a JSON object
// decodes into a Feed with zero entries — identity's own /jwks.json does,
// verbatim, and it sits one path segment away on the same host and port. A
// gateway configured one character off would prime successfully, export
// kanz_api_gateway_revocations_usable=1, and go on honouring every revoked token
// on the platform indefinitely.
//
// That is exactly "nothing configured" and "checked, and nobody is revoked"
// producing the same observable result, on the one control where the difference
// is the entire point. A discriminator turns it into a startup failure that
// names the URL.
//
// IT IS VERSIONED because the day the wire shape changes, a gateway running the
// old reader must refuse the new feed rather than silently read fewer entries
// out of it.
const FeedKind = "kanz.revocations.v1"
