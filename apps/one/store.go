package main

import "lab/apps/one/repo"

// The database lives in package repo, the only code that touches it.
type store = repo.Store

func openStore(path string) (*store, error) { return repo.Open(path) }
