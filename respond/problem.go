package respond

import (
	"strings"

	"github.com/Donk3ys/kit/apperr"
)

// ProblemContentType is the media type RFC 9457 requires for problem details,
// distinct from the application/json used for ordinary responses. Clients key
// error handling off it, so it is not interchangeable with application/json.
const ProblemContentType = "application/problem+json"

// reservedMembers are the members this package owns. RFC 9457 extension
// members are top-level siblings of these, so an extension named "status"
// would otherwise silently overwrite the real status and desynchronise the
// body from the response line.
var reservedMembers = map[string]struct{}{
	"type":     {},
	"title":    {},
	"status":   {},
	"detail":   {},
	"instance": {},
	"code":     {},
}

// baseProblem builds the members this package owns, without extensions.
func (b *Boundary) baseProblem(e *apperr.Error, status int, instance string) map[string]any {
	body := map[string]any{
		"type":   b.typeURI(e.Code),
		"title":  statusTitle(status),
		"status": status,
		"code":   e.Code,
	}
	if e.SafeDetail != "" {
		body["detail"] = e.SafeDetail
	}
	// instance identifies this occurrence. The request ID is what a user can
	// actually quote back to support, which makes it more useful here than a
	// synthetic URI.
	if instance != "" {
		body["instance"] = instance
	}
	return body
}

// problemWithExtensions merges an error's extension members over the base
// body, skipping any that would overwrite a reserved member.
func (b *Boundary) problemWithExtensions(
	e *apperr.Error, status int, instance string,
) (body map[string]any, skipped []string) {
	body = b.baseProblem(e, status, instance)
	for k, v := range e.Extensions {
		if _, reserved := reservedMembers[k]; reserved {
			skipped = append(skipped, k)
			continue
		}
		body[k] = v
	}
	return body, skipped
}

// typeURI returns the problem "type" member. RFC 9457 intends it to be a URI
// that documents the problem class; "about:blank" is the defined fallback
// when there is nothing to point at.
func (b *Boundary) typeURI(code string) string {
	if b.TypeBaseURI == "" || code == "" {
		return "about:blank"
	}
	return strings.TrimSuffix(b.TypeBaseURI, "/") + "/" + codeSlug(code)
}

// codeSlug renders a screaming-snake code as a URL path segment:
// PROFILE_NOT_FOUND becomes profile-not-found.
func codeSlug(code string) string {
	return strings.ToLower(strings.ReplaceAll(code, "_", "-"))
}
