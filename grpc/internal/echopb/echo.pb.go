// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.5
// 	protoc        v5.30.0--dev
// source: echo.proto

package echopb

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type EchoRequest struct {
	state                  protoimpl.MessageState `protogen:"opaque.v1"`
	xxx_hidden_Msg         *string                `protobuf:"bytes,1,opt,name=msg"`
	XXX_raceDetectHookData protoimpl.RaceDetectHookData
	XXX_presence           [1]uint32
	unknownFields          protoimpl.UnknownFields
	sizeCache              protoimpl.SizeCache
}

func (x *EchoRequest) Reset() {
	*x = EchoRequest{}
	mi := &file_echo_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EchoRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EchoRequest) ProtoMessage() {}

func (x *EchoRequest) ProtoReflect() protoreflect.Message {
	mi := &file_echo_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *EchoRequest) GetMsg() string {
	if x != nil {
		if x.xxx_hidden_Msg != nil {
			return *x.xxx_hidden_Msg
		}
		return ""
	}
	return ""
}

func (x *EchoRequest) SetMsg(v string) {
	x.xxx_hidden_Msg = &v
	protoimpl.X.SetPresent(&(x.XXX_presence[0]), 0, 1)
}

func (x *EchoRequest) HasMsg() bool {
	if x == nil {
		return false
	}
	return protoimpl.X.Present(&(x.XXX_presence[0]), 0)
}

func (x *EchoRequest) ClearMsg() {
	protoimpl.X.ClearPresent(&(x.XXX_presence[0]), 0)
	x.xxx_hidden_Msg = nil
}

type EchoRequest_builder struct {
	_ [0]func() // Prevents comparability and use of unkeyed literals for the builder.

	Msg *string
}

func (b0 EchoRequest_builder) Build() *EchoRequest {
	m0 := &EchoRequest{}
	b, x := &b0, m0
	_, _ = b, x
	if b.Msg != nil {
		protoimpl.X.SetPresentNonAtomic(&(x.XXX_presence[0]), 0, 1)
		x.xxx_hidden_Msg = b.Msg
	}
	return m0
}

type EchoResponse struct {
	state                  protoimpl.MessageState `protogen:"opaque.v1"`
	xxx_hidden_Msg         *string                `protobuf:"bytes,1,opt,name=msg"`
	XXX_raceDetectHookData protoimpl.RaceDetectHookData
	XXX_presence           [1]uint32
	unknownFields          protoimpl.UnknownFields
	sizeCache              protoimpl.SizeCache
}

func (x *EchoResponse) Reset() {
	*x = EchoResponse{}
	mi := &file_echo_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EchoResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EchoResponse) ProtoMessage() {}

func (x *EchoResponse) ProtoReflect() protoreflect.Message {
	mi := &file_echo_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

func (x *EchoResponse) GetMsg() string {
	if x != nil {
		if x.xxx_hidden_Msg != nil {
			return *x.xxx_hidden_Msg
		}
		return ""
	}
	return ""
}

func (x *EchoResponse) SetMsg(v string) {
	x.xxx_hidden_Msg = &v
	protoimpl.X.SetPresent(&(x.XXX_presence[0]), 0, 1)
}

func (x *EchoResponse) HasMsg() bool {
	if x == nil {
		return false
	}
	return protoimpl.X.Present(&(x.XXX_presence[0]), 0)
}

func (x *EchoResponse) ClearMsg() {
	protoimpl.X.ClearPresent(&(x.XXX_presence[0]), 0)
	x.xxx_hidden_Msg = nil
}

type EchoResponse_builder struct {
	_ [0]func() // Prevents comparability and use of unkeyed literals for the builder.

	Msg *string
}

func (b0 EchoResponse_builder) Build() *EchoResponse {
	m0 := &EchoResponse{}
	b, x := &b0, m0
	_, _ = b, x
	if b.Msg != nil {
		protoimpl.X.SetPresentNonAtomic(&(x.XXX_presence[0]), 0, 1)
		x.xxx_hidden_Msg = b.Msg
	}
	return m0
}

var File_echo_proto protoreflect.FileDescriptor

var file_echo_proto_rawDesc = string([]byte{
	0x0a, 0x0a, 0x65, 0x63, 0x68, 0x6f, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x13, 0x68, 0x75,
	0x6d, 0x75, 0x73, 0x2e, 0x67, 0x72, 0x70, 0x63, 0x2e, 0x69, 0x6e, 0x74, 0x65, 0x72, 0x6e, 0x61,
	0x6c, 0x22, 0x1f, 0x0a, 0x0b, 0x45, 0x63, 0x68, 0x6f, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74,
	0x12, 0x10, 0x0a, 0x03, 0x6d, 0x73, 0x67, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x03, 0x6d,
	0x73, 0x67, 0x22, 0x20, 0x0a, 0x0c, 0x45, 0x63, 0x68, 0x6f, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e,
	0x73, 0x65, 0x12, 0x10, 0x0a, 0x03, 0x6d, 0x73, 0x67, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52,
	0x03, 0x6d, 0x73, 0x67, 0x32, 0x53, 0x0a, 0x04, 0x45, 0x63, 0x68, 0x6f, 0x12, 0x4b, 0x0a, 0x04,
	0x45, 0x63, 0x68, 0x6f, 0x12, 0x20, 0x2e, 0x68, 0x75, 0x6d, 0x75, 0x73, 0x2e, 0x67, 0x72, 0x70,
	0x63, 0x2e, 0x69, 0x6e, 0x74, 0x65, 0x72, 0x6e, 0x61, 0x6c, 0x2e, 0x45, 0x63, 0x68, 0x6f, 0x52,
	0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a, 0x21, 0x2e, 0x68, 0x75, 0x6d, 0x75, 0x73, 0x2e, 0x67,
	0x72, 0x70, 0x63, 0x2e, 0x69, 0x6e, 0x74, 0x65, 0x72, 0x6e, 0x61, 0x6c, 0x2e, 0x45, 0x63, 0x68,
	0x6f, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x42, 0x2e, 0x5a, 0x2c, 0x67, 0x69, 0x74,
	0x68, 0x75, 0x62, 0x2e, 0x63, 0x6f, 0x6d, 0x2f, 0x7a, 0x35, 0x6c, 0x61, 0x62, 0x73, 0x2f, 0x68,
	0x75, 0x6d, 0x75, 0x73, 0x2f, 0x67, 0x72, 0x70, 0x63, 0x2f, 0x69, 0x6e, 0x74, 0x65, 0x72, 0x6e,
	0x61, 0x6c, 0x2f, 0x65, 0x63, 0x68, 0x6f, 0x70, 0x62, 0x62, 0x08, 0x65, 0x64, 0x69, 0x74, 0x69,
	0x6f, 0x6e, 0x73, 0x70, 0xe8, 0x07,
})

var file_echo_proto_msgTypes = make([]protoimpl.MessageInfo, 2)
var file_echo_proto_goTypes = []any{
	(*EchoRequest)(nil),  // 0: humus.grpc.internal.EchoRequest
	(*EchoResponse)(nil), // 1: humus.grpc.internal.EchoResponse
}
var file_echo_proto_depIdxs = []int32{
	0, // 0: humus.grpc.internal.Echo.Echo:input_type -> humus.grpc.internal.EchoRequest
	1, // 1: humus.grpc.internal.Echo.Echo:output_type -> humus.grpc.internal.EchoResponse
	1, // [1:2] is the sub-list for method output_type
	0, // [0:1] is the sub-list for method input_type
	0, // [0:0] is the sub-list for extension type_name
	0, // [0:0] is the sub-list for extension extendee
	0, // [0:0] is the sub-list for field type_name
}

func init() { file_echo_proto_init() }
func file_echo_proto_init() {
	if File_echo_proto != nil {
		return
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_echo_proto_rawDesc), len(file_echo_proto_rawDesc)),
			NumEnums:      0,
			NumMessages:   2,
			NumExtensions: 0,
			NumServices:   1,
		},
		GoTypes:           file_echo_proto_goTypes,
		DependencyIndexes: file_echo_proto_depIdxs,
		MessageInfos:      file_echo_proto_msgTypes,
	}.Build()
	File_echo_proto = out.File
	file_echo_proto_goTypes = nil
	file_echo_proto_depIdxs = nil
}
