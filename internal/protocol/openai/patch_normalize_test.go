package openai

import (
	"testing"
)

func TestNormalizePatchInput_NonPatchTool(t *testing.T) {
	// Non-patch tools should pass through unchanged.
	input := `{"command": ["ls", "-la"]}`
	result := normalizePatchInput("exec", input)
	if result != input {
		t.Errorf("exec tool should not be normalized, got %q", result)
	}
}

func TestNormalizePatchInput_AlreadyCorrect(t *testing.T) {
	input := `{"input": "*** Begin Patch\n--- a/file\n+++ b/file\n@@ -1,3 +1,5 @@\n hello\n+world\n*** End Patch"}`
	result := normalizePatchInput("apply_patch", input)
	if result != input {
		t.Errorf("already-correct patch should pass through, got %q", result)
	}
}

func TestNormalizePatchInput_AddFileMarker(t *testing.T) {
	// DeepSeek sometimes generates *** Add File marker instead of *** Begin Patch.
	input := `{"input": "*** Add File /tmp/test.py\nprint('hello')\nprint('world')"}`
	result := normalizePatchInput("apply_patch", input)

	expected := `{"input":"*** Begin Patch\nprint('hello')\nprint('world')\n*** End Patch"}`
	if result != expected {
		t.Errorf("Add File marker not normalized.\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNormalizePatchInput_ModifyFileMarker(t *testing.T) {
	input := `{"input": "*** Modify File /tmp/test.py\n--- a/test.py\n+++ b/test.py\n@@ -1 +1,2 @@\n-hello\n+hello\n+world"}`
	result := normalizePatchInput("apply_patch", input)

	// The *** Modify File marker is stripped, and the unified diff gets wrapped.
	expected := `{"input":"*** Begin Patch\n--- a/test.py\n+++ b/test.py\n@@ -1 +1,2 @@\n-hello\n+hello\n+world\n*** End Patch"}`
	if result != expected {
		t.Errorf("Modify File marker not normalized.\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNormalizePatchInput_BareUnifiedDiff(t *testing.T) {
	// Model generates a clean unified diff but doesn't wrap it.
	input := `{"input": "--- a/src/app.py\n+++ b/src/app.py\n@@ -10,3 +10,5 @@\n old\n+new\n more"}`
	result := normalizePatchInput("apply_patch", input)

	expected := `{"input":"*** Begin Patch\n--- a/src/app.py\n+++ b/src/app.py\n@@ -10,3 +10,5 @@\n old\n+new\n more\n*** End Patch"}`
	if result != expected {
		t.Errorf("bare unified diff not wrapped.\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNormalizePatchInput_DiffGitPrefix(t *testing.T) {
	input := `{"input": "diff --git a/file.txt b/file.txt\nindex abc..def\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new"}`
	result := normalizePatchInput("apply_patch", input)

	expected := `{"input":"*** Begin Patch\ndiff --git a/file.txt b/file.txt\nindex abc..def\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n*** End Patch"}`
	if result != expected {
		t.Errorf("diff --git format not wrapped.\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNormalizePatchInput_PlainStringNotJSON(t *testing.T) {
	// Model might output a plain string instead of JSON object.
	input := "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new"
	result := normalizePatchInput("apply_patch", input)

	expected := "*** Begin Patch\n--- a/file\n+++ b/file\n@@ -1 +1 @@\n-old\n+new\n*** End Patch"
	if result != expected {
		t.Errorf("plain string diff not wrapped.\ngot:  %q\nwant: %q", result, expected)
	}
}

func TestNormalizePatchInput_EmptyInput(t *testing.T) {
	// Empty or null input should pass through.
	tests := []string{"", "{}"}
	for _, input := range tests {
		result := normalizePatchInput("apply_patch", input)
		if result != input {
			t.Errorf("empty input %q should pass through, got %q", input, result)
		}
	}
}

func TestNormalizePatchInput_PatchToolNameVariant(t *testing.T) {
	// Both "apply_patch" and "patch" should be normalized.
	tests := []string{"apply_patch", "patch", "Apply_Patch", "PATCH"}
	for _, name := range tests {
		input := `{"input": "--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b"}`
		result := normalizePatchInput(name, input)
		if result == input {
			t.Errorf("tool %q should have been normalized but passed through", name)
		}
	}
}

func TestNormalizePatchContent_NonPatchContent(t *testing.T) {
	// Non-patch content should pass through unchanged.
	content := "hello world"
	result := normalizePatchContent(content)
	if result != content {
		t.Errorf("non-patch content should not change, got %q", result)
	}
}

func TestNormalizePatchContent_DeleteFileMarker(t *testing.T) {
	content := "*** Delete File /tmp/old.py\n--- a/old.py\n+++ /dev/null"
	result := normalizePatchContent(content)

	expected := "*** Begin Patch\n--- a/old.py\n+++ /dev/null\n*** End Patch"
	if result != expected {
		t.Errorf("Delete File marker not normalized.\ngot:  %q\nwant: %q", result, expected)
	}
}
