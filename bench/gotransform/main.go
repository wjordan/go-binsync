// gotransform: experiments for a Go-aware predict-then-correct delta transform.
//
//	gotransform inventory OLD [NEW]
//	gotransform sectdiff OLD NEW
//	gotransform predict OLD NEW
//	gotransform pclnpredict OLD NEW
//	gotransform datapredict OLD NEW
//	gotransform whole OLD NEW
//	gotransform encode [-s1 h|b|z] [-s2 p|h|b|z] OLD NEW PATCH
//	gotransform decode OLD PATCH NEW
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}

func main() {
	// encode/decode hold two or three whole binaries; a lower GC target keeps
	// the reported peak RSS closer to the live set (override with GOGC).
	if len(os.Args) > 1 && (os.Args[1] == "encode" || os.Args[1] == "decode") {
		if os.Getenv("GOGC") == "" {
			debug.SetGCPercent(50)
		}
		// soft limit: the live set is ~5x the binary, the transient
		// content-map indexes would otherwise let the heap grow past it
		if os.Getenv("GOMEMLIMIT") == "" {
			debug.SetMemoryLimit(640 << 20)
		}
	}
	if len(os.Args) < 2 {
		fatal("usage: gotransform {inventory|sectdiff|predict|pclnpredict|datapredict} ...")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "inventory":
		cmdInventory(args)
	case "sectdiff":
		cmdSectdiff(args)
	case "predict":
		cmdPredict(args)
	case "pclnpredict":
		cmdPclnPredict(args)
	case "datapredict":
		cmdDataPredict(args)
	case "whole":
		cmdWhole(args)
	case "encode":
		cmdEncode(args)
	case "decode":
		cmdDecode(args)
	case "blobcheck":
		for _, a := range args {
			b, err := loadBin(a)
			must(err)
			fmt.Printf("%s: %s\n", a, selfCheckBlobs(b))
		}
	default:
		fatal("unknown command %q", os.Args[1])
	}
}
