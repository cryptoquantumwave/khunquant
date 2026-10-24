package identity

import "strings"

// CanonicalSender returns sender as "platform:id". A sender already written in
// that form is rebuilt (platform lower-cased, both parts trimmed); a raw id is
// qualified with fallbackPlatform, the way the allow-list falls back. It returns
// "" when no canonical form can be built.
func CanonicalSender(sender, fallbackPlatform string) string {
	if platform, id, ok := ParseCanonicalID(sender); ok {
		return BuildCanonicalID(platform, id)
	}
	return BuildCanonicalID(fallbackPlatform, sender)
}

// SameSender reports whether two canonical sender IDs name the same person: both
// non-empty and equal under Unicode case folding. It is the one comparison that
// decides "is this the configured owner" for a tool call (tools.checkOwnerTurn)
// and for an approval decision (approval.Service.Decide), so the two cannot
// disagree about who the owner is. An empty side never matches, so no owner
// configured means nobody is.
func SameSender(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(a, b)
}

// IsOwner reports whether sender is the owner whose canonical ID is ownerID:
// sender is made canonical first (see CanonicalSender) and compared with
// SameSender.
func IsOwner(sender, fallbackPlatform, ownerID string) bool {
	return SameSender(CanonicalSender(sender, fallbackPlatform), ownerID)
}
