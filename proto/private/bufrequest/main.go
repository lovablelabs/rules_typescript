// Command bufrequest re-encodes protoc's CodeGeneratorRequest as buf generate
// does, then runs protoc-gen-es on it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/pluginpb"
)

var pluginRlocationpath string

func main() {
	if err := run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "bufrequest: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	protoc, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	request, err := bufRequest(protoc)
	if err != nil {
		return err
	}
	rf, err := runfiles.New()
	if err != nil {
		return err
	}
	plugin, err := rf.Rlocation(pluginRlocationpath)
	if err != nil {
		return err
	}
	cmd := exec.Command(plugin)
	cmd.Env = append(os.Environ(), rf.Env()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(request), os.Stdout, os.Stderr
	return cmd.Run()
}

// Mirrors buf v1.73.0: bufimage.resolverForFDS, bufimageutil's remapOptions
// and bufprotopluginexec.binaryHandler's deterministic marshal.
func bufRequest(protoc []byte) ([]byte, error) {
	request := new(pluginpb.CodeGeneratorRequest)
	if err := proto.Unmarshal(protoc, request); err != nil {
		return nil, err
	}
	files, err := protodesc.FileOptions{AllowUnresolvable: true}.NewFiles(&descriptorpb.FileDescriptorSet{File: request.GetProtoFile()})
	if err != nil {
		return nil, err
	}
	request.Reset()
	if err := (proto.UnmarshalOptions{Resolver: dynamicpb.NewTypes(files)}).Unmarshal(protoc, request); err != nil {
		return nil, err
	}
	for _, file := range slices.Concat(request.GetProtoFile(), request.GetSourceFileDescriptors()) {
		dropEmptyOptions(file.ProtoReflect())
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(request)
}

func dropEmptyOptions(descriptor protoreflect.Message) {
	var empty []protoreflect.FieldDescriptor
	descriptor.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.Message() == nil:
		case field.IsList():
			for i := range value.List().Len() {
				dropEmptyOptions(value.List().Get(i).Message())
			}
		case field.Name() == "options":
			if !hasField(value.Message()) {
				empty = append(empty, field)
			}
		default:
			dropEmptyOptions(value.Message())
		}
		return true
	})
	for _, field := range empty {
		descriptor.Clear(field)
	}
}

func hasField(message protoreflect.Message) bool {
	found := false
	message.Range(func(protoreflect.FieldDescriptor, protoreflect.Value) bool {
		found = true
		return false
	})
	return found
}
