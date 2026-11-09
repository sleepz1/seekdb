package db

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// Active bounded-mode transactions retain SQL on disk, not a second in-memory
// copy of every inserted payload. Spools are never recovery input.
func (s *Session) recordStatement(sql string) error {
	if s.e.pages == nil {
		s.statements = append(s.statements, sql)
		s.txWrites++
		return nil
	}
	var f *os.File
	var err error
	if s.spool == "" {
		f, err = os.CreateTemp(filepath.Join(s.e.dir, "segments"), "tx-*.pending")
		if err == nil {
			s.spool = f.Name()
		}
	} else {
		f, err = os.OpenFile(s.spool, os.O_WRONLY|os.O_APPEND, 0600)
	}
	if err != nil {
		return err
	}
	off, err := f.Seek(0, 2)
	if err != nil {
		f.Close()
		return err
	}
	err = json.NewEncoder(f).Encode(sql)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		if truncErr := os.Truncate(s.spool, off); truncErr != nil {
			s.e.walErr = truncErr
		}
	}
	if err == nil {
		s.txWrites++
	}
	return err
}
func (s *Session) clearStatements() {
	if s.spool != "" {
		os.Remove(s.spool)
	}
	s.spool = ""
	s.txWrites = 0
	s.statements = nil
}
func (s *Session) persistStatements() error {
	if s.txWrites == 0 {
		return nil
	}
	if s.spool == "" {
		return s.e.persist(s.statements)
	}
	f, err := os.Open(s.spool)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.e.persistBatch(func(emit func(string) error) error {
		d := json.NewDecoder(f)
		for {
			var stmt string
			err := d.Decode(&stmt)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err = emit(stmt); err != nil {
				return err
			}
		}
	})
}
