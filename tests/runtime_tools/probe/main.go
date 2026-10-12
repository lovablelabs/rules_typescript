// Command probe is the executable the runtime tools tests hand to a program.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Println("probe ran with " + strings.Join(os.Args[1:], " "))
}
