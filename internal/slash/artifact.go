package slash

import "strings"

// RegisterArtifactCommands installs the model-directed Artifact workflows.
// Storage and preview remain owned by the Artifact tool so slash dispatch uses
// the ordinary tool permission, persistence, and structured-result path.
func RegisterArtifactCommands(r *Registry) {
	r.Register(Cmd{
		Name:         "artifact",
		Description:  "create, update, or read a durable HTML artifact",
		ArgumentHint: "<request>",
		Category:     "artifact",
		Handler: func(args string) (string, Signal) {
			return buildArtifactPrompt(strings.TrimSpace(args)), SignalCustomPrompt
		},
	})
	r.Register(Cmd{
		Name:        "artifacts",
		Description: "list artifacts attached to the current session",
		Category:    "artifact",
		Handler: func(_ string) (string, Signal) {
			return buildArtifactListPrompt(), SignalCustomPrompt
		},
	})
}

func buildArtifactPrompt(request string) string {
	var b strings.Builder
	b.WriteString("# Artifact request\n\n")
	b.WriteString("Use the `Artifact` tool to carry out this request. ")
	b.WriteString("Do not simulate artifact creation or mutation with a prose-only answer, and do not treat an ordinary workspace file as a registered artifact. ")
	b.WriteString("Use the tool's list or read action first when an artifact ID is needed. ")
	b.WriteString("The model-facing tool supports create, update, list, and read only; exporting and deleting are explicit user actions handled by Desktop or `metis artifacts export|delete`. ")
	b.WriteString("After the tool succeeds, report the artifact ID, title, version, and the available follow-up action.\n")
	if request == "" {
		b.WriteString("\nThe user did not provide a specific operation. Use the `Artifact` tool to list artifacts for the current session, then briefly explain its create, update, and read actions. Mention that export and delete require `metis artifacts ...` or Desktop.\n")
	} else {
		b.WriteString("\n## User request\n\n")
		b.WriteString(request)
		b.WriteByte('\n')
	}
	return b.String()
}

func buildArtifactListPrompt() string {
	return "# List artifacts\n\n" +
		"Use the `Artifact` tool with its list action for the current session. " +
		"Present a compact list containing each artifact's ID, title, version, and updated time. " +
		"If the list is empty, say so and tell the user they can run `/artifact <request>` to create one. " +
		"Do not invent artifacts or infer them only from workspace filenames.\n"
}
