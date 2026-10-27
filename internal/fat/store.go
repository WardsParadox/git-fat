package fat

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// ObjectMode is the permission set for stored objects.
func ObjectMode() os.FileMode {
	return os.FileMode(0o444 &^ umask())
}

// Catalog returns the names of all objects in the local store.
func (r *Repo) Catalog() (map[string]struct{}, error) {
	entries, err := os.ReadDir(r.ObjDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if validDigest(e.Name()) {
			set[e.Name()] = struct{}{}
		}
	}
	return set, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify prints objects whose content does not match their name.
func (r *Repo) Verify() error {
	cat, err := r.Catalog()
	if err != nil {
		return err
	}
	type bad struct{ name, hash string }
	var corrupted []bad
	for _, name := range sortedKeys(cat) {
		sum, err := hashFile(filepath.Join(r.ObjDir, name))
		if err != nil {
			return err
		}
		if sum != name {
			corrupted = append(corrupted, bad{name, sum})
		}
	}
	if len(corrupted) == 0 {
		return nil
	}
	fmt.Printf("Corrupted objects: %d\n", len(corrupted))
	for _, c := range corrupted {
		fmt.Printf("%s data hash is %s\n", c.name, c.hash)
	}
	return fmt.Errorf("%d corrupted objects", len(corrupted))
}

// GC removes objects that no reachable commit references.
func (r *Repo) GC(ctx context.Context) error {
	cat, err := r.Catalog()
	if err != nil {
		return err
	}
	refs, err := r.Referenced(ctx, "", false)
	if err != nil {
		return err
	}
	var garbage []string
	for name := range cat {
		if _, ok := refs[name]; !ok {
			garbage = append(garbage, name)
		}
	}
	sort.Strings(garbage)
	fmt.Printf("Unreferenced objects to remove: %d\n", len(garbage))
	for _, name := range garbage {
		path := filepath.Join(r.ObjDir, name)
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		fmt.Printf("%10d %s\n", fi.Size(), name)
		if err := removeObject(path); err != nil {
			return err
		}
	}
	return nil
}

// Status reports objects that are missing locally or unreferenced.
func (r *Repo) Status(ctx context.Context, all bool) error {
	if err := r.Setup(); err != nil {
		return err
	}
	cat, err := r.Catalog()
	if err != nil {
		return err
	}
	refs, err := r.Referenced(ctx, "", all)
	if err != nil {
		return err
	}
	if all {
		for _, name := range sortedKeys(refs) {
			fmt.Println(name)
		}
	}
	var orphans, garbage []string
	for name := range refs {
		if _, ok := cat[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	for name := range cat {
		if _, ok := refs[name]; !ok {
			garbage = append(garbage, name)
		}
	}
	sort.Strings(orphans)
	sort.Strings(garbage)
	if len(orphans) > 0 {
		fmt.Println("Orphan objects:")
		for _, o := range orphans {
			fmt.Println("    " + o)
		}
	}
	if len(garbage) > 0 {
		fmt.Println("Garbage objects:")
		for _, g := range garbage {
			fmt.Println("    " + g)
		}
	}
	return nil
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
