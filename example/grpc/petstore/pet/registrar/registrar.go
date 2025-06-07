// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package registrar

import (
	"context"

	"github.com/z5labs/humus/example/grpc/petstore/petpb"
	"github.com/z5labs/humus/example/internal/pet"

	"github.com/z5labs/sdk-go/ptr"
	"google.golang.org/grpc"
)

type Store interface {
	Register(context.Context, *pet.RegisterRequest) (*pet.RegisterResponse, error)
}

type Registrar struct {
	petpb.UnimplementedRegistrarServer

	store Store
}

func Register(sr grpc.ServiceRegistrar, store Store) {
	r := &Registrar{
		store: store,
	}

	petpb.RegisterRegistrarServer(sr, r)
}

func (r *Registrar) RegisterPet(ctx context.Context, req *petpb.RegisterPetRequest) (*petpb.RegisterPetResponse, error) {
	registerResp, err := r.store.Register(ctx, &pet.RegisterRequest{
		Age:   req.GetAge(),
		Name:  req.GetName(),
		Kind:  req.GetKind().String(),
		Breed: req.GetBreed(),
		Fur: pet.FurDesc{
			Kind:  req.GetFur().GetKind().String(),
			Color: req.GetFur().GetColor(),
		},
	})
	if err != nil {
		return nil, err
	}

	builder := petpb.RegisterPetResponse_builder{
		Id: ptr.Ref(registerResp.ID),
	}
	if len(registerResp.TempName) > 0 {
		builder.TempName = ptr.Ref(registerResp.TempName)
	}
	return builder.Build(), nil
}
