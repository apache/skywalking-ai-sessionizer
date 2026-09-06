{{.LicenseContent }}
{{ range .Groups }}
========================================================================
{{.LicenseID}} licenses
========================================================================
{{range .Deps}}
    {{.Name}} {{.Version}} {{.LicenseID}}
{{- end }}
{{ end }}
========================================================================
OFL-1.1 licenses
========================================================================

The conversation renderer the asz viewer embeds carries two fonts, under the
SIL Open Font License, Version 1.1; their texts are in licenses/.

    Inter (github.com/rsms/inter) OFL-1.1, licenses/license-inter-font.txt
    JetBrains Mono (github.com/JetBrains/JetBrainsMono) OFL-1.1, licenses/license-jetbrains-mono-font.txt
