// action-guard: decides whether a remediation playbook may run (community edition).
package main

import (
	"os"

	"github.com/ventrolab/ai-sre/services/action-guard/guard"
)

func main() {
	if err := guard.Run(guard.Options{}); err != nil {
		os.Exit(1)
	}
}
