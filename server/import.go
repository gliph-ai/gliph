package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// importSQLite copies a SQLite gliph database into this (PostgreSQL) store,
// once: only when the destination has no accounts yet, and in one transaction,
// so a failed import leaves nothing behind and the next boot tries again.
//
// It runs at boot, before the server listens. Booting on an empty database
// first and importing after would leave a window in which the first person to
// register becomes the admin of gliph.sh.
func importSQLite(st *Store, src string) error {
	var users int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return err
	}
	if users > 0 {
		return nil // already imported, or a live server: never merge into one
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	from, err := OpenStore(src) // brings the source's columns up to date too
	if err != nil {
		return err
	}
	defer from.Close()
	return copyTables(from, st)
}

// copyTables copies every row of every table from one store into the other,
// empty, one, in one transaction. One side must be SQLite: its sqlite_master
// is the table list, in creation order, which is dependency order (schema.sql
// defines a table after the tables it references). -import-sqlite copies
// SQLite into PostgreSQL; a backup of a PostgreSQL server copies the other way.
func copyTables(from, to *Store) error {
	from.retroSchema()
	to.retroSchema()
	lite := from
	if from.db.pg {
		lite = to
	}
	var tables []string
	rows, err := lite.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY rowid`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t string
		rows.Scan(&t)
		tables = append(tables, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	tx, err := to.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range tables {
		rows, err := from.db.Query(`SELECT * FROM ` + t)
		if err != nil {
			return fmt.Errorf("import %s: %w", t, err)
		}
		cols, _ := rows.Columns()
		ins := `INSERT INTO ` + t + `(` + strings.Join(cols, ",") + `) VALUES(` +
			strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + `)`
		n := 0
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return fmt.Errorf("import %s: %w", t, err)
			}
			if _, err := tx.Exec(ins, vals...); err != nil {
				rows.Close()
				return fmt.Errorf("import %s: %w", t, err)
			}
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("import %s: %w", t, err)
		}
		log.Printf("import: %s, %d rows", t, n)
	}
	return tx.Commit()
}

// importFiles moves what the SQLite era kept on disk into the bucket: every
// upload (an old row holds an absolute path, a newer one a key relative to the
// data dir) and the voice notes under <data>/roomnotes.
// A row is repointed only after its bytes are stored, so a failure leaves it
// on disk for the next boot to retry. A file that is gone is left alone: its
// row 404s, as it did before.
func importFiles(st *Store, f files, dataDir string) error {
	b, ok := f.(*s3Files)
	if !ok {
		return nil
	}
	put := func(key, path string) (bool, error) {
		fh, err := os.Open(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		defer fh.Close()
		fi, err := fh.Stat()
		if err != nil {
			return false, err
		}
		return true, b.write(key, 0, fh, fi.Size())
	}
	rows, err := st.db.Query(`SELECT id,path FROM uploads`)
	if err != nil {
		return err
	}
	type up struct{ id, path string }
	var ups []up
	for rows.Next() {
		var u up
		rows.Scan(&u.id, &u.path)
		ups = append(ups, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, u := range ups {
		key, disk := u.path, filepath.Join(dataDir, filepath.FromSlash(u.path))
		if filepath.IsAbs(u.path) {
			key, disk = "uploads/"+filepath.Base(u.path), u.path
		}
		moved, err := put(key, disk)
		if err != nil {
			return fmt.Errorf("import upload %s: %w", u.id, err)
		}
		if moved && key != u.path {
			if _, err := st.db.Exec(`UPDATE uploads SET path=? WHERE id=?`, key, u.id); err != nil {
				return fmt.Errorf("import upload %s: %w", u.id, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "roomnotes"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if _, err := put(noteKey(e.Name()), filepath.Join(dataDir, "roomnotes", e.Name())); err != nil {
			return fmt.Errorf("import voice note %s: %w", e.Name(), err)
		}
	}
	log.Printf("import: %d uploads, %d voice notes into the bucket", len(ups), len(entries))
	return nil
}

// importIntoPG is the -import-sqlite step: the database, then any files the
// SQLite era kept beside it. The store it opens is closed again before the
// server opens its own.
func importIntoPG(pg string, bucket *s3Files, src string) error {
	if pg == "" {
		return errors.New("-import-sqlite needs DATABASE_URL to point at PostgreSQL")
	}
	st, err := OpenStore(pg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := importSQLite(st, src); err != nil {
		return err
	}
	if bucket == nil {
		return nil
	}
	return importFiles(st, bucket, filepath.Dir(src))
}
