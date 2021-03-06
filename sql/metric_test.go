// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Cuong Do (cdo@cockroachlabs.com)

package sql_test

import (
	"bytes"
	"testing"

	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/sql"
	"github.com/cockroachdb/cockroach/storage/storagebase"
	"github.com/cockroachdb/cockroach/testutils"
	"github.com/cockroachdb/cockroach/testutils/serverutils"
	"github.com/cockroachdb/cockroach/util/leaktest"
)

func TestQueryCounts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	params, _ := createTestServerParams()
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop()

	var testcases = []struct {
		query            string
		txnBeginCount    int64
		selectCount      int64
		updateCount      int64
		insertCount      int64
		deleteCount      int64
		ddlCount         int64
		miscCount        int64
		txnCommitCount   int64
		txnRollbackCount int64
	}{
		{"", 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{"BEGIN; END", 1, 0, 0, 0, 0, 0, 0, 1, 0},
		{"SELECT 1", 1, 1, 0, 0, 0, 0, 0, 1, 0},
		{"CREATE DATABASE mt", 1, 1, 0, 0, 0, 1, 0, 1, 0},
		{"CREATE TABLE mt.n (num INTEGER)", 1, 1, 0, 0, 0, 2, 0, 1, 0},
		{"INSERT INTO mt.n VALUES (3)", 1, 1, 0, 1, 0, 2, 0, 1, 0},
		{"UPDATE mt.n SET num = num + 1", 1, 1, 1, 1, 0, 2, 0, 1, 0},
		{"DELETE FROM mt.n", 1, 1, 1, 1, 1, 2, 0, 1, 0},
		{"ALTER TABLE mt.n ADD COLUMN num2 INTEGER", 1, 1, 1, 1, 1, 3, 0, 1, 0},
		{"EXPLAIN SELECT * FROM mt.n", 1, 1, 1, 1, 1, 3, 1, 1, 0},
		{"BEGIN; UPDATE mt.n SET num = num + 1; END", 2, 1, 2, 1, 1, 3, 1, 2, 0},
		{"SELECT * FROM mt.n; SELECT * FROM mt.n; SELECT * FROM mt.n", 2, 4, 2, 1, 1, 3, 1, 2, 0},
		{"DROP TABLE mt.n", 2, 4, 2, 1, 1, 4, 1, 2, 0},
		{"SET database = system", 2, 4, 2, 1, 1, 4, 2, 2, 0},
	}

	for _, tc := range testcases {
		if tc.query != "" {
			if _, err := sqlDB.Exec(tc.query); err != nil {
				t.Fatalf("unexpected error executing '%s': %s'", tc.query, err)
			}
		}

		// Force metric snapshot refresh.
		if err := s.WriteSummaries(); err != nil {
			t.Fatal(err)
		}

		checkCounterEQ(t, s, sql.MetricTxnBeginName, tc.txnBeginCount)
		checkCounterEQ(t, s, sql.MetricTxnCommitName, tc.txnCommitCount)
		checkCounterEQ(t, s, sql.MetricTxnRollbackName, tc.txnRollbackCount)
		checkCounterEQ(t, s, sql.MetricTxnAbortName, 0)
		checkCounterEQ(t, s, sql.MetricSelectName, tc.selectCount)
		checkCounterEQ(t, s, sql.MetricUpdateName, tc.updateCount)
		checkCounterEQ(t, s, sql.MetricInsertName, tc.insertCount)
		checkCounterEQ(t, s, sql.MetricDeleteName, tc.deleteCount)
		checkCounterEQ(t, s, sql.MetricDdlName, tc.ddlCount)
		checkCounterEQ(t, s, sql.MetricMiscName, tc.miscCount)

		// Everything after this query will also fail, so quit now to avoid deluge of errors.
		if t.Failed() {
			t.FailNow()
		}
	}
}

func TestAbortCountConflictingWrites(t *testing.T) {
	defer leaktest.AfterTest(t)()

	params, cmdFilters := createTestServerParams()
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop()

	if _, err := sqlDB.Exec("CREATE DATABASE db"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec("CREATE TABLE db.t (k TEXT PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatal(err)
	}

	// Inject errors on the INSERT below.
	restarted := false
	cmdFilters.AppendFilter(func(args storagebase.FilterArgs) *roachpb.Error {
		switch req := args.Req.(type) {
		// SQL INSERT generates ConditionalPuts for unique indexes (such as the PK).
		case *roachpb.ConditionalPutRequest:
			if bytes.Contains(req.Value.RawBytes, []byte("marker")) && !restarted {
				restarted = true
				return roachpb.NewErrorWithTxn(
					roachpb.NewTransactionAbortedError(), args.Hdr.Txn)
			}
		}
		return nil
	}, false)

	txn, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = txn.Exec("INSERT INTO db.t VALUES ('key', 'marker')")
	if !testutils.IsError(err, "aborted") {
		t.Fatal(err)
	}

	if err = txn.Rollback(); err != nil {
		t.Fatal(err)
	}

	checkCounterEQ(t, s, sql.MetricTxnAbortName, 1)
	checkCounterEQ(t, s, sql.MetricTxnBeginName, 1)
	checkCounterEQ(t, s, sql.MetricTxnRollbackName, 0)
	checkCounterEQ(t, s, sql.MetricTxnCommitName, 0)
	checkCounterEQ(t, s, sql.MetricInsertName, 1)
}

// TestErrorDuringTransaction tests that the transaction abort count goes up when a query
// results in an error during a txn.
func TestAbortCountErrorDuringTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)()
	params, _ := createTestServerParams()
	s, sqlDB, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop()

	txn, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := txn.Query("SELECT * FROM i_do.not_exist"); err == nil {
		t.Fatal("Expected an error but didn't get one")
	}

	checkCounterEQ(t, s, sql.MetricTxnAbortName, 1)
	checkCounterEQ(t, s, sql.MetricTxnBeginName, 1)
	checkCounterEQ(t, s, sql.MetricSelectName, 1)
}
