package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// RunRecord is a host-checkable execution record. The answer is derived from
// actual step outputs, never from the model's final explanation.
type RunRecord struct {
	Name         string       `json:"name"`
	RecipeSHA256 string       `json:"recipe_sha256"`
	Steps        []StepResult `json:"steps"`
}

var evaluationFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var evaluationInteger = regexp.MustCompile(`^[0-9]{1,12}$`)

// ValidateEvaluationRecipe deliberately supports a bounded, portable recipe
// subset. The ordinary Workflow tool still supports builds and arbitrary
// authorized shell commands; learning cannot grade those without a stronger
// fixture/read boundary. Unsupported candidates fail closed before execution.
func ValidateEvaluationRecipe(wf Workflow) error {
	return ValidateEvaluationFixtureRecipe(wf, nil)
}

// ValidateEvaluationFixtureRecipe extends the default policy only for an
// explicit host fixture. Source writes remain bounded and the final command
// invokes the immutable host tests rather than a candidate-supplied program.
func ValidateEvaluationFixtureRecipe(wf Workflow, fixture *EvaluationFixtureContext) error {
	if fixture != nil && (len(wf.Steps) == 0 || strings.TrimSpace(wf.Steps[len(wf.Steps)-1].Command) != "fixture-test") {
		return errors.New("fixture evaluation requires fixture-test as its final step")
	}
	for _, step := range wf.Steps {
		if err := validateEvaluationFixtureCommand(step.Command, fixture); err != nil {
			return fmt.Errorf("workflow evaluation step %q: %w", step.Name, err)
		}
	}
	return nil
}
func validateEvaluationCommand(command string) error {
	return validateEvaluationFixtureCommand(command, nil)
}
func validateEvaluationFixtureCommand(command string, fixture *EvaluationFixtureContext) error {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "")
	if err != nil {
		return err
	}
	if len(f.Stmts) != 1 {
		return errors.New("evaluation permits one simple command per step")
	}
	s := f.Stmts[0]
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) != 0 || len(c.Args) == 0 || s.Background || s.Coprocess || s.Disown || s.Negated {
		return errors.New("compound commands, assignments and background execution are unavailable in evaluation")
	}
	name, err := evaluationWord(c.Args[0], false)
	if err != nil {
		return err
	}
	args := make([]string, 0, len(c.Args)-1)
	for _, w := range c.Args[1:] {
		value, err := evaluationWord(w, true)
		if err != nil {
			return err
		}
		args = append(args, value)
	}
	switch name {
	case "fixture-test":
		if fixture == nil || len(args) != 0 || len(s.Redirs) != 0 {
			return errors.New("fixture-test requires a host fixture and takes no arguments or redirection")
		}
	case "printf":
		if len(args) == 0 {
			return errors.New("printf format required")
		}
		// No width, precision or dynamic formats: huge formatted outputs could
		// exhaust the parent before its normal output cap takes effect.
		for i := 0; i < len(args[0]); i++ {
			if args[0][i] == '%' {
				i++
				if i >= len(args[0]) || !strings.ContainsRune("%sdiu", rune(args[0][i])) {
					return errors.New("evaluation printf permits only %s, %d, %i, %u and %%")
				}
			}
		}
	case "echo":
	case "true", "false":
		if len(args) != 0 {
			return errors.New("evaluation true/false take no arguments")
		}
	case "expr":
		if len(args) != 3 || !evaluationInteger.MatchString(args[0]) || !evaluationInteger.MatchString(args[2]) || !strings.Contains(" + - * / % ", " "+args[1]+" ") {
			return errors.New("evaluation expr requires two integer operands and a numeric operator")
		}
	case "cat":
		if len(args) == 0 {
			return errors.New("evaluation cat requires private workspace filenames")
		}
		for _, arg := range args {
			if !evaluationFile.MatchString(arg) || strings.Contains(arg, "..") {
				return errors.New("evaluation cat permits only private workspace filenames")
			}
		}
	default:
		return errors.New("evaluation command unavailable; use bounded printf/echo/expr/true/false/cat recipes")
	}
	if len(s.Redirs) > 1 {
		return errors.New("evaluation permits at most one output redirection")
	}
	for _, r := range s.Redirs {
		if r.N != nil || r.Hdoc != nil || (r.Op.String() != ">" && r.Op.String() != ">>") {
			return errors.New("evaluation permits only stdout file redirection")
		}
		file, err := evaluationWord(r.Word, false)
		if err != nil || !evaluationFile.MatchString(file) || strings.Contains(file, "..") {
			return errors.New("evaluation output must be a private workspace filename")
		}
		if fixture != nil && !fixture.CanWrite(file) {
			return errors.New("fixture evaluation may only redirect to host-declared source files")
		}
	}
	return nil
}
func evaluationWord(w *syntax.Word, arithmetic bool) (string, error) {
	return evaluationParts(w.Parts, arithmetic, false)
}
func evaluationParts(parts []syntax.WordPart, arithmetic, quoted bool) (string, error) {
	var b strings.Builder
	for _, p := range parts {
		switch x := p.(type) {
		case *syntax.Lit:
			if !quoted && strings.ContainsAny(x.Value, "~*?[{\\$") {
				return "", errors.New("evaluation disallows glob, tilde and escaped word expansion")
			}
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			if x.Dollar {
				return "", errors.New("evaluation disallows dollar quoting")
			}
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			if x.Dollar {
				return "", errors.New("evaluation disallows locale quoting")
			}
			value, err := evaluationParts(x.Parts, arithmetic, true)
			if err != nil {
				return "", err
			}
			b.WriteString(value)
		case *syntax.ArithmExp:
			if !arithmetic || x.Bracket || x.Unsigned || !evaluationArithmetic(x.X) {
				return "", errors.New("evaluation permits numeric arithmetic without variables")
			}
			b.WriteString("0")
		default:
			return "", errors.New("evaluation disallows variables, substitutions and dynamic expansion")
		}
	}
	if b.Len() > 8192 {
		return "", errors.New("evaluation argument too large")
	}
	return b.String(), nil
}
func evaluationArithmetic(x syntax.ArithmExpr) bool {
	switch x := x.(type) {
	case *syntax.Word:
		return len(x.Parts) == 1 && func() bool {
			lit, ok := x.Parts[0].(*syntax.Lit)
			return ok && evaluationInteger.MatchString(lit.Value)
		}()
	case *syntax.BinaryArithm:
		return strings.Contains(" + - * / % ", " "+x.Op.String()+" ") && evaluationArithmetic(x.X) && evaluationArithmetic(x.Y)
	case *syntax.UnaryArithm:
		return !x.Post && (x.Op.String() == "+" || x.Op.String() == "-") && evaluationArithmetic(x.X)
	case *syntax.ParenArithm:
		return evaluationArithmetic(x.X)
	default:
		return false
	}
}
