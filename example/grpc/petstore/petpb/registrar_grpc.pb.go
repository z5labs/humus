// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.5.1
// - protoc             v5.30.0--dev
// source: proto/registrar.proto

package petpb

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.64.0 or later.
const _ = grpc.SupportPackageIsVersion9

const (
	Registrar_RegisterPet_FullMethodName = "/humus.example.pet.Registrar/RegisterPet"
)

// RegistrarClient is the client API for Registrar service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type RegistrarClient interface {
	RegisterPet(ctx context.Context, in *RegisterPetRequest, opts ...grpc.CallOption) (*RegisterPetResponse, error)
}

type registrarClient struct {
	cc grpc.ClientConnInterface
}

func NewRegistrarClient(cc grpc.ClientConnInterface) RegistrarClient {
	return &registrarClient{cc}
}

func (c *registrarClient) RegisterPet(ctx context.Context, in *RegisterPetRequest, opts ...grpc.CallOption) (*RegisterPetResponse, error) {
	cOpts := append([]grpc.CallOption{grpc.StaticMethod()}, opts...)
	out := new(RegisterPetResponse)
	err := c.cc.Invoke(ctx, Registrar_RegisterPet_FullMethodName, in, out, cOpts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RegistrarServer is the server API for Registrar service.
// All implementations must embed UnimplementedRegistrarServer
// for forward compatibility.
type RegistrarServer interface {
	RegisterPet(context.Context, *RegisterPetRequest) (*RegisterPetResponse, error)
	mustEmbedUnimplementedRegistrarServer()
}

// UnimplementedRegistrarServer must be embedded to have
// forward compatible implementations.
//
// NOTE: this should be embedded by value instead of pointer to avoid a nil
// pointer dereference when methods are called.
type UnimplementedRegistrarServer struct{}

func (UnimplementedRegistrarServer) RegisterPet(context.Context, *RegisterPetRequest) (*RegisterPetResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RegisterPet not implemented")
}
func (UnimplementedRegistrarServer) mustEmbedUnimplementedRegistrarServer() {}
func (UnimplementedRegistrarServer) testEmbeddedByValue()                   {}

// UnsafeRegistrarServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to RegistrarServer will
// result in compilation errors.
type UnsafeRegistrarServer interface {
	mustEmbedUnimplementedRegistrarServer()
}

func RegisterRegistrarServer(s grpc.ServiceRegistrar, srv RegistrarServer) {
	// If the following call pancis, it indicates UnimplementedRegistrarServer was
	// embedded by pointer and is nil.  This will cause panics if an
	// unimplemented method is ever invoked, so we test this at initialization
	// time to prevent it from happening at runtime later due to I/O.
	if t, ok := srv.(interface{ testEmbeddedByValue() }); ok {
		t.testEmbeddedByValue()
	}
	s.RegisterService(&Registrar_ServiceDesc, srv)
}

func _Registrar_RegisterPet_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(RegisterPetRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(RegistrarServer).RegisterPet(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: Registrar_RegisterPet_FullMethodName,
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(RegistrarServer).RegisterPet(ctx, req.(*RegisterPetRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// Registrar_ServiceDesc is the grpc.ServiceDesc for Registrar service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var Registrar_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "humus.example.pet.Registrar",
	HandlerType: (*RegistrarServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "RegisterPet",
			Handler:    _Registrar_RegisterPet_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "proto/registrar.proto",
}
