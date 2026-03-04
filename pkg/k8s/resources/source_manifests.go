package resources

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/vercel/bridge/pkg/archive"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

// SourceFromManifests unpacks a tar.gz archive of Kubernetes YAML files, parses
// every document into typed or unstructured resources, and returns a Bundle
// ready for the transform pipeline.
func SourceFromManifests(manifests []byte) (*Bundle, error) {
	fileMap, err := archive.Unpack(manifests)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack source manifests: %w", err)
	}

	var resources []Resource

	decoder := scheme.Codecs.UniversalDeserializer()

	for name, content := range fileMap {
		docs := splitYAMLDocuments(content)
		for i, doc := range docs {
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}

			obj, gvk, err := decoder.Decode(doc, nil, nil)
			if err != nil {
				// Try as unstructured for unknown types.
				u, uErr := decodeUnstructured(doc)
				if uErr != nil {
					slog.Warn("Skipping unparseable YAML document", "file", name, "doc", i, "error", err)
					continue
				}
				resources = append(resources, Resource{Object: u, GVK: u.GroupVersionKind()})
				continue
			}

			switch obj.(type) {
			case *appsv1.Deployment:
				resources = append(resources, Resource{Object: obj, GVK: *gvk})
			case *corev1.Service, *corev1.ConfigMap, *corev1.Secret, *corev1.ServiceAccount:
				resources = append(resources, Resource{Object: obj, GVK: *gvk})
			default:
				slog.Debug("Parsed known but unhandled resource type", "gvk", gvk, "file", name, "doc", i)
				u, uErr := decodeUnstructured(doc)
				if uErr == nil {
					resources = append(resources, Resource{Object: u, GVK: u.GroupVersionKind()})
				}
			}
		}
	}

	return &Bundle{
		Resources: resources,
	}, nil
}

// splitYAMLDocuments splits multi-document YAML (separated by ---) into individual documents.
func splitYAMLDocuments(data []byte) [][]byte {
	reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// If we can't split, treat the whole thing as one document.
			return [][]byte{data}
		}
		docs = append(docs, doc)
	}
	return docs
}

// decodeUnstructured parses raw YAML into an Unstructured object.
func decodeUnstructured(data []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	if err := dec.Decode(obj); err != nil {
		return nil, err
	}
	if obj.GetKind() == "" {
		return nil, fmt.Errorf("decoded object has no kind")
	}
	return obj, nil
}

// gvkToGVR converts a GroupVersionKind to a GroupVersionResource using a
// simple pluralization heuristic (lowercase kind + "s").
func gvkToGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	plural := strings.ToLower(gvk.Kind) + "s"
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: plural,
	}
}
