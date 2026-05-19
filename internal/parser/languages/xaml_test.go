package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestXAMLExtractor_ClassControlsAndBindings(t *testing.T) {
	src := []byte(`<Window x:Class="MyApp.MainWindow"
        xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml">
    <Grid>
        <Button x:Name="submitButton" Content="Submit" />
        <TextBlock x:Name="statusLabel" Text="{Binding StatusMessage}" />
    </Grid>
</Window>`)
	res, err := NewXAMLExtractor().Extract("MainWindow.xaml", src)
	require.NoError(t, err)

	var file *graph.Node
	controls := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		switch n.Kind {
		case graph.KindFile:
			file = n
		case graph.KindVariable:
			controls[n.Name] = n
		}
	}
	require.NotNil(t, file)
	require.Equal(t, "MyApp.MainWindow", file.Meta["xaml_class"])

	require.Len(t, controls, 2)
	require.Equal(t, "Button", controls["submitButton"].Meta["xaml_element"])

	bindings, _ := controls["statusLabel"].Meta["xaml_bindings"].([]string)
	require.Len(t, bindings, 1)
	require.Contains(t, bindings[0], "{Binding StatusMessage}")
}

func TestXAMLExtractor_Malformed(t *testing.T) {
	res, err := NewXAMLExtractor().Extract("bad.xaml", []byte(`<Window x:Name="w"><Grid`))
	require.NoError(t, err)        // never a hard failure
	require.NotEmpty(t, res.Nodes) // at least the file node survives
}

func TestXAMLExtractor_Extensions(t *testing.T) {
	require.Equal(t, []string{".xaml", ".axaml"}, NewXAMLExtractor().Extensions())
}
