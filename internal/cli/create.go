package cli

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/kurenn/dossier-cli/internal/api"
	"github.com/kurenn/dossier-cli/internal/render"
	"github.com/spf13/cobra"
)

type createOptions struct {
	label       string
	category    string
	country     string
	sensitivity int

	// sensitivitySet distinguishes "--sensitivity 0", which the server will reject, from
	// "no --sensitivity", where the server picks the default. Without it the flag's zero
	// value and a deliberate zero are the same request.
	sensitivitySet bool

	valueFile  string
	valueStdin bool

	// valueFlag exists to be refused. See valueRefusal.
	valueFlag string
}

func newFieldsCreateCmd(app *App) *cobra.Command {
	var opts createOptions

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Add a custom field to your vault",
		Long: "Creates one custom field. The label, category, country and sensitivity are\n" +
			"metadata; the value is the secret, and it is the one thing this command will\n" +
			"not take as an argument.\n\n" +
			"The value comes from a hidden prompt, from --value-stdin, or from --value-file.\n" +
			"An argument would be recorded in your shell history and visible in `ps` to\n" +
			"every other process on the machine for as long as the command ran.\n\n" +
			"The value is never printed back: not on success, not in an error, not under\n" +
			"--json. The API does not return it and this command has nowhere to put one.\n\n" +
			"There is no Idempotency-Key on this endpoint, so a failure you cannot read —\n" +
			"a timeout, a dropped connection — is never retried. Run `dossier fields list`\n" +
			"and look for the label before trying again.",
		Args: refuseValueAsArgument,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.sensitivitySet = cmd.Flags().Changed("sensitivity")
			return app.runFieldsCreate(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&opts.label, "label", "", "What the field is called in your vault (required)")
	cmd.Flags().StringVar(&opts.category, "category", "", "Which section it files under (see: dossier schema)")
	cmd.Flags().StringVar(&opts.country, "country", "", "ISO3 country code, if the field belongs to one")
	cmd.Flags().IntVar(&opts.sensitivity, "sensitivity", 0, "How guarded the value is; the API's range is in: dossier schema")
	cmd.Flags().StringVar(&opts.valueFile, "value-file", "", "Read the value from this file")
	cmd.Flags().BoolVar(&opts.valueStdin, "value-stdin", false, "Read the value from standard input")

	// Hidden, because --help should show the three ways that are safe and not advertise a
	// fourth that is not. It is declared at all so that someone who reaches for the
	// obvious flag is told why it does not exist, instead of getting cobra's "unknown
	// flag" and concluding the CLI simply forgot it.
	cmd.Flags().StringVar(&opts.valueFlag, "value", "", "Refused; see --value-stdin and --value-file")
	_ = cmd.Flags().MarkHidden("value")

	return cmd
}

// valueRefusal is the message for every attempt to pass the value on the command line.
//
// Exit 2, and nothing is sent. The plan's M2 makes this a done-when rather than a nicety:
// a secret on a command line is in the shell's history file and in every `ps` listing on
// the box, and neither is undone by the process exiting.
const valueRefusal = "A field's value cannot be passed on the command line.\n\n" +
	"It would be written to your shell history and visible in `ps` to every other\n" +
	"process on this machine while the command ran.\n\n" +
	"Use one of:\n\n" +
	"    dossier fields create --label \"CURP\"                  # hidden prompt\n" +
	"    dossier fields create --label \"CURP\" --value-stdin    # from a pipe\n" +
	"    dossier fields create --label \"CURP\" --value-file ./curp.txt\n"

func refuseValueAsArgument(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return Usagef("%s", valueRefusal)
	}
	return nil
}

func (a *App) runFieldsCreate(ctx context.Context, opts createOptions) error {
	if opts.valueFlag != "" {
		return Usagef("%s", valueRefusal)
	}
	if strings.TrimSpace(opts.label) == "" {
		return Usagef("A field needs a --label.\n\n    dossier fields create --label \"CURP\"\n")
	}

	resolution, err := a.Resolve()
	if err != nil {
		return err
	}
	if err := a.requireToken(resolution); err != nil {
		return err
	}
	client, err := a.Client(resolution)
	if err != nil {
		return err
	}

	// Both checks happen before the value is even read, so a mistyped category costs a
	// cached schema read rather than a prompt the holder has to answer twice.
	schema, _, _, err := a.schema(ctx, client, false)
	if err != nil {
		return err
	}
	if err := validateVocabulary("category", opts.category, schema.FieldCategories); err != nil {
		return err
	}

	value, err := a.readValue(opts)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return Usagef("The value is empty. A field with no value is not created.\n")
	}

	input := api.CreateFieldInput{
		Label:    strings.TrimSpace(opts.label),
		Value:    value,
		Category: opts.category,
		Country:  opts.country,
	}
	if opts.sensitivitySet {
		input.Sensitivity = &opts.sensitivity
	}

	field, raw, err := client.CreateField(ctx, input, a.timeout())
	if err != nil {
		return a.classifyWrite(err, "creating the field", "dossier fields list")
	}

	if a.JSON {
		a.emitRawPage(raw)
		return nil
	}

	a.renderFieldBlock(*field)
	a.Notef("\nAdded to the vault. The value is stored encrypted and is not shown here.\n")
	return nil
}

// readValue gets the secret from exactly one of the three permitted sources.
//
// The order is explicit rather than a fallback chain: a holder who passed --value-file
// and also piped something in has contradicted themselves, and guessing which they meant
// is how the wrong secret gets stored under the right label.
func (a *App) readValue(opts createOptions) (string, error) {
	switch {
	case opts.valueFile != "" && opts.valueStdin:
		return "", Usagef("Choose one of --value-file and --value-stdin, not both.\n")

	case opts.valueFile != "":
		content, err := os.ReadFile(opts.valueFile)
		if err != nil {
			return "", Usagef("Could not read the value file: %s\n", err)
		}
		return strings.TrimSpace(string(content)), nil

	case opts.valueStdin:
		value, err := a.readSecret("")
		if err != nil {
			return "", err
		}
		return value, nil

	case a.interactive():
		return a.readSecret("Value (not echoed): ")

	default:
		// A script with no source named. Refused rather than left waiting on a stdin
		// nobody is going to type into, which is how a CI job hangs until its timeout.
		return "", Usagef(
			"No value, and nothing to prompt — standard input is not a terminal.\n\n" +
				"Pass --value-stdin to read it from the pipe, or --value-file to read it\n" +
				"from a file.\n")
	}
}

// classifyWrite adds the one thing an API envelope cannot know: that this endpoint has no
// idempotency guard, so an ambiguous failure must not be retried blind.
//
// Only for failures that are not the API refusing — a 422 or a 403 arrived, which means
// the server decided and nothing was written, and telling a holder to go and check after
// a clean refusal would be noise. A transport failure is the ambiguous one: the request
// may have been received, executed and its response lost.
func (a *App) classifyWrite(err error, doing, check string) error {
	cliErr := Classify(err)
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return cliErr
	}

	cliErr.Message = cliErr.Message + "\n\n" +
		"This did not get a reply, so there is no way to tell from here whether the\n" +
		"server acted on it. It has not been retried: this endpoint takes no\n" +
		"Idempotency-Key, so a second attempt is a second write, not a repeat of the\n" +
		"first.\n\n" +
		"Run `" + check + "` to see what happened before " + doing + " again.\n"
	return cliErr
}

// renderFieldBlock prints one field as a record.
//
// The same keys as a row of `fields list`, in the same order, because they are the same
// object — a holder who creates a field and then lists it must not have to work out that
// two differently-shaped outputs describe one thing. DOCS is omitted: a freshly created
// field has no documents and the API says so with an empty array, so a column reading 0
// would be the only content of a row that exists to be a placeholder.
func (a *App) renderFieldBlock(field api.Field) {
	block := render.NewBlock(len("SENSITIVITY"))
	block.Add("ID", strconv.Itoa(field.ID))
	block.Add("LABEL", field.Label)
	block.Add("CATEGORY", field.Category)
	block.Add("COUNTRY", derefOr(field.Country, ""))
	block.Add("SENSITIVITY", strconv.Itoa(field.Sensitivity))
	block.Add("STATUS", field.Status)
	block.Add("HAS_DOCUMENT", strconv.FormatBool(field.HasDocument))
	_ = block.Render(a.Stdout)
}
