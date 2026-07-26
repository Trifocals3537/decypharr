package storage

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestEntryItemsReconcileAfterUncleanShutdown(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := entryItemTransactionEntry(
		"entry-item-recovery",
		"expected-folder",
		"expected-file",
	)
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	if err := store.entryItems.Delete(entry.GetFolder()); err != nil {
		t.Fatal(err)
	}
	orphan := &EntryItem{
		Name: "orphan-folder",
		Files: map[string]*File{
			"orphan-file": {
				InfoHash: "missing-entry",
				Name:     "orphan-file",
			},
		},
	}
	orphanData, err := proto.Marshal(EntryItemToProto(orphan))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.entryItems.Put(orphan.Name, orphanData, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.entryItems.Sync(); err != nil {
		t.Fatal(err)
	}
	store.startupComplete = false
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close recovered storage: %v", err)
		}
	})

	item, err := store.GetEntryItem(entry.GetFolder())
	if err != nil {
		t.Fatal(err)
	}
	if file := item.Files["expected-file"]; file == nil ||
		file.InfoHash != entry.InfoHash {
		t.Fatalf("recovered entry item = %#v", item)
	}
	if store.entryItems.Exists(orphan.Name) {
		t.Fatal("orphan entry item survived unclean-shutdown recovery")
	}
}

func TestEntryItemsRecoveryPreservesDurableDeletedFiles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "db")
	store, err := NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	entry := entryItemTransactionEntry(
		"entry-item-deleted",
		"deleted-folder",
		"deleted-file",
	)
	entry.Files["deleted-file"].Size = 123
	if err := store.AddOrUpdate(entry); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetEntryItem(entry.GetFolder())
	if err != nil {
		t.Fatal(err)
	}
	item.Files["deleted-file"].Deleted = true
	item.Size = item.GetSize()
	if err := store.UpdateItem(item); err != nil {
		t.Fatal(err)
	}
	if err := store.entryItems.Sync(); err != nil {
		t.Fatal(err)
	}
	store.startupComplete = false
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewStorage(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close recovered storage: %v", err)
		}
	})
	item, err = store.GetEntryItem(entry.GetFolder())
	if err != nil {
		t.Fatal(err)
	}
	if file := item.Files["deleted-file"]; file == nil || !file.Deleted {
		t.Fatalf("recovery lost durable deleted flag: %#v", item)
	}
	if item.Size != 0 {
		t.Fatalf("recovered deleted item size = %d, want 0", item.Size)
	}
}
