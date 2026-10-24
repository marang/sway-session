package session

// A non-writable directory remains replaceable by its owner. User launchers
// therefore trust only ancestors controlled by root or the approved file owner.
func trustedDesktopPathOwner(owner uint32, approvedOwner uint32) bool {
	return owner == 0 || owner == approvedOwner
}
