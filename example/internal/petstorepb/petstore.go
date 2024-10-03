// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package petstorepb

import (
	"github.com/z5labs/humus/rest"
	"google.golang.org/protobuf/proto"
)

func (*AddPetRequest) ContentType() string {
	return rest.ProtobufContentType
}

func (req *AddPetRequest) UnmarshalBinary(b []byte) error {
	return proto.Unmarshal(b, req)
}

func (*AddPetResponse) ContentType() string {
	return rest.ProtobufContentType
}

func (resp *AddPetResponse) MarshalBinary() ([]byte, error) {
	return proto.Marshal(resp)
}

func (*FindPetByIdResponse) ContentType() string {
	return rest.ProtobufContentType
}

func (resp *FindPetByIdResponse) MarshalBinary() ([]byte, error) {
	return proto.Marshal(resp)
}

func (*ListPetsResponse) ContentType() string {
	return rest.ProtobufContentType
}

func (resp *ListPetsResponse) MarshalBinary() ([]byte, error) {
	return proto.Marshal(resp)
}
