// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health

import (
	"context"
	"fmt"
)

func ExampleBinary() {
	var b Binary

	healthy, _ := b.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = b.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkUnhealthy()

	healthy, _ = b.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// true
	// false
}

func ExampleAnd() {
	var a Binary
	var b Binary

	and := And(&a, &b)

	healthy, _ := and.Healthy(context.Background())
	fmt.Println(healthy)

	a.MarkHealthy()

	healthy, _ = and.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = and.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// false
	// true
}

func ExampleOr() {
	var a Binary
	var b Binary

	or := Or(&a, &b)

	healthy, _ := or.Healthy(context.Background())
	fmt.Println(healthy)

	a.MarkHealthy()

	healthy, _ = or.Healthy(context.Background())
	fmt.Println(healthy)

	b.MarkHealthy()

	healthy, _ = or.Healthy(context.Background())
	fmt.Println(healthy)

	// Output: false
	// true
	// true
}
