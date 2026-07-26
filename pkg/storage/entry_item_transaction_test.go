package storage

import (
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/pkg/storage/hybrid"
)

func TestMainEntryWriteFailureRollsBackNewEntryItem(t *testing.T) {
	store := newMainLifecycleTestStorage(t)
	store.startupComplete = false
	if err := store.entries.Close(); err != nil {
		t.Fatal(err)
	}

	entry := entryItemTransactionEntry("new-main", "new-folder", "new-file")
	if err := store.AddOrUpdate(entry); !errors.Is(err, hybrid.ErrStoreClosed) {
		t.Fatalf("AddOrUpdate() error = %v, want ErrStoreClosed", err)
	}
	if store.entryItems.Exists(entry.GetFolder()) {
		t.Fatal("failed authoritative write left a new secondary entry item")
	}
}

func TestMainEntrySecondaryFailureLeavesAuthoritativeRowUnchanged(t *testing.T) {
	store := newMainLifecycleTestStorage(t)
	entry := entryItemTransactionEntry("existing-main", "existing-folder", "old-file")
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	store.startupComplete = false
	if err := store.entryItems.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot.Files["new-file"] = &File{InfoHash: snapshot.InfoHash, Name: "new-file"}

	if err := store.AddOrUpdate(snapshot); !errors.Is(err, hybrid.ErrStoreClosed) {
		t.Fatalf("AddOrUpdate() error = %v, want ErrStoreClosed", err)
	}
	current, err := store.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := current.Files["new-file"]; exists {
		t.Fatal("secondary write failure committed the authoritative main update")
	}
}

func TestMainEntryDeleteSecondaryFailureRetainsAuthoritativeRow(t *testing.T) {
	store := newMainLifecycleTestStorage(t)
	entry := entryItemTransactionEntry("delete-main", "delete-folder", "file")
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	store.startupComplete = false
	if err := store.entryItems.Close(); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(entry.InfoHash); !errors.Is(err, hybrid.ErrStoreClosed) {
		t.Fatalf("Delete() error = %v, want ErrStoreClosed", err)
	}
	if _, err := store.Get(entry.InfoHash); err != nil {
		t.Fatalf("Get() after failed delete error = %v", err)
	}
	if _, found, err := store.loadMainEntryTombstone(entry.InfoHash); err != nil || found {
		t.Fatalf("delete tombstone after failed delete = found %v, error %v", found, err)
	}
}

func TestMainEntryFolderChangeRepairsBothEntryItems(t *testing.T) {
	store := newMainLifecycleTestStorage(t)
	entry := entryItemTransactionEntry("move-main", "old-folder", "file")
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Get(entry.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Name = "new-folder"
	if err := store.AddOrUpdate(snapshot); err != nil {
		t.Fatal(err)
	}

	if store.entryItems.Exists("old-folder") {
		t.Fatal("old entry item survived a main-entry folder change")
	}
	item, err := store.GetEntryItem("new-folder")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := item.Files["file"]; !exists {
		t.Fatal("new entry item is missing the moved main-entry file")
	}
}

func entryItemTransactionEntry(key, folder, fileName string) *Entry {
	return &Entry{
		InfoHash: key,
		Name:     folder,
		Files: map[string]*File{
			fileName: {
				InfoHash: key,
				Name:     fileName,
			},
		},
	}
}
