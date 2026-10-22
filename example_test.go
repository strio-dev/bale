package bale_test

import (
	"fmt"

	"github.com/strio-dev/bale"
)

func ExampleNormalizeRecipe() {
	recipe, err := bale.NormalizeRecipe(bale.Recipe{
		Include: []bale.Selection{{Ref: "registry.example.com/team/component:v1"}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(recipe.Include[0].Referrers.Include)
	// Output: all
}
