package cel2yaml_test

import (
	"context"
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"

	"github.com/okdp/cel2yaml"
)

// Render: a document on a scope, written as YAML.
func ExampleRender() {
	scope := cel2yaml.Scope{"data": map[string]any{"test": 1}}
	out, err := cel2yaml.Render(context.Background(), []byte("a: ${{ data.test }}\n"), scope)
	fmt.Printf("%s%v\n", out, err)
	// Output:
	// a: 1
	// <nil>
}

// Func: a host function of the caller's, in a session.
func ExampleFunc() {
	shout := cel2yaml.Func("shout", cel.StringType, func(v any) (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("not a string: %v", v)
		}
		return strings.ToUpper(s) + "!", nil
	})
	s := cel2yaml.NewSession(context.Background(), cel2yaml.DefaultCostLimit, shout)
	out, err := s.Render([]byte("greeting: ${{ shout(name) }}\n"), cel2yaml.Scope{"name": "hello"})
	fmt.Printf("%s%v\n", out, err)
	// Output:
	// greeting: HELLO!
	// <nil>
}

// Template: the parts of a document compiled together, then evaluated.
func ExampleSession_Template() {
	s := cel2yaml.NewSession(context.Background(), cel2yaml.DefaultCostLimit)
	parse := func(src string) *cel2yaml.Template {
		n, err := cel2yaml.Parse([]byte(src))
		if err != nil {
			panic(err)
		}
		t, err := s.Template(n, []string{"x"})
		if err != nil {
			panic(err)
		}
		return t
	}
	a, b := parse("double: ${{ x * 2.0 }}\n"), parse("{$if: '${{ x > 5.0 }}', big: true}\n")
	_ = s.Document(func() error {
		for _, x := range []float64{1, 10} {
			for _, t := range []*cel2yaml.Template{a, b} {
				v, keep, err := t.Eval(cel2yaml.Scope{"x": x})
				fmt.Println(v, keep, err)
			}
		}
		return nil
	})
	// Output:
	// map[double:2] true <nil>
	// <nil> false <nil>
	// map[double:20] true <nil>
	// map[big:true] true <nil>
}
