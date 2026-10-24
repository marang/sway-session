package statefile

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestStateDirectoryAncestorEntriesAlwaysCheckOwnership(t *testing.T) {
	userID := uint32(os.Geteuid())
	foreignID := userID + 1
	if foreignID == 0 {
		foreignID = 1
	}
	for name, test := range map[string]struct {
		parentOwner uint32
		childOwner  uint32
		parentMode  os.FileMode
		wantErr     bool
	}{
		"root ordinary parent":    {parentOwner: 0, childOwner: userID, parentMode: 0o755},
		"user ordinary parent":    {parentOwner: userID, childOwner: userID, parentMode: 0o755},
		"user private parent":     {parentOwner: userID, childOwner: 0, parentMode: 0o700},
		"foreign ordinary parent": {parentOwner: foreignID, childOwner: userID, parentMode: 0o755, wantErr: true},
		"foreign private parent":  {parentOwner: foreignID, childOwner: 0, parentMode: 0o700, wantErr: true},
		"foreign ordinary child":  {parentOwner: userID, childOwner: foreignID, parentMode: 0o755, wantErr: true},
		"root sticky parent":      {parentOwner: 0, childOwner: userID, parentMode: 0o777 | os.ModeSticky},
		"user sticky parent":      {parentOwner: userID, childOwner: userID, parentMode: 0o777 | os.ModeSticky},
		"foreign sticky parent":   {parentOwner: foreignID, childOwner: userID, parentMode: 0o777 | os.ModeSticky, wantErr: true},
		"foreign sticky child":    {parentOwner: 0, childOwner: foreignID, parentMode: 0o777 | os.ModeSticky, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			parent := ancestorOwnershipInfo{mode: os.ModeDir | test.parentMode, system: &syscall.Stat_t{Uid: test.parentOwner}}
			child := ancestorOwnershipInfo{mode: os.ModeDir | DirectoryMode, system: &syscall.Stat_t{Uid: test.childOwner}}
			if err := checkStateDirectoryAncestorEntry(parent, child); (err != nil) != test.wantErr {
				t.Fatalf("ancestor ownership check returned %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestStateDirectoryAncestorEntriesRequireOwnershipMetadata(t *testing.T) {
	trusted := ancestorOwnershipInfo{mode: os.ModeDir | 0o755, system: &syscall.Stat_t{Uid: uint32(os.Geteuid())}}
	missing := ancestorOwnershipInfo{mode: os.ModeDir | 0o755}
	for name, test := range map[string]struct {
		parent os.FileInfo
		child  os.FileInfo
	}{
		"parent": {parent: missing, child: trusted},
		"child":  {parent: trusted, child: missing},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkStateDirectoryAncestorEntry(test.parent, test.child); err == nil {
				t.Fatal("ancestor with unavailable ownership metadata was trusted")
			}
		})
	}
}

type ancestorOwnershipInfo struct {
	mode   os.FileMode
	system any
}

func (info ancestorOwnershipInfo) Name() string       { return "ancestor" }
func (info ancestorOwnershipInfo) Size() int64        { return 0 }
func (info ancestorOwnershipInfo) Mode() os.FileMode  { return info.mode }
func (info ancestorOwnershipInfo) ModTime() time.Time { return time.Time{} }
func (info ancestorOwnershipInfo) IsDir() bool        { return info.mode.IsDir() }
func (info ancestorOwnershipInfo) Sys() any           { return info.system }
