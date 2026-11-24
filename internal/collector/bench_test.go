// Copyright (C) 2026 Yukthi Systems Private Limited
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License version 3
// as published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// version 3 along with this program. If not, see
// <https://www.gnu.org/licenses/>.

package collector

import (
	"bufio"
	"os"
	"testing"

	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/journal"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/postfix"
	"github.com/Yukthi-Systems/Phoenix-MailStatus-Collector/internal/tracker"
)

// BenchmarkPipeline measures decode + parse + track per journal line.
// Input: MAILSTATUS_BENCH_FILE (a journalctl -o json export).
func BenchmarkPipeline(b *testing.B) {
	path := os.Getenv("MAILSTATUS_BENCH_FILE")
	if path == "" {
		b.Skip("MAILSTATUS_BENCH_FILE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, append([]byte(nil), sc.Bytes()...))
	}
	f.Close()

	p := postfix.NewParser("X-Ref-ID")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tr := tracker.New(tracker.Config{SuccessCode: "255"}, quiet)
		for _, l := range lines {
			e, err := journal.Decode(l)
			if err != nil {
				b.Fatal(err)
			}
			tr.Handle(e, p.Parse(e.Message))
		}
		tr.Snapshot()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(lines)), "ns/line")
}
