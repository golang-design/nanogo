// A method of a generic type. nanogo instantiates a generic function and it
// does not instantiate a generic type, so nothing produces the body of
// box[int].get, and the package is refused by name.
package main

import "os"

type box[T any] struct{ v T }

func (b box[T]) get() T { return b.v }

func main() {
	os.Exit(box[int]{7}.get())
}
