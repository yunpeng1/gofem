// Copyright 2015 Dorival Pedroso and Raul Durand. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package msolid

import (
	"github.com/cpmech/gosl/fun"
	"github.com/cpmech/gosl/io"
	"github.com/cpmech/gosl/la"
	"github.com/cpmech/gosl/num"
	"github.com/cpmech/gosl/tsr"
	"github.com/cpmech/gosl/utl"
)

// PrincStrainsUp implements stress-update in principal strains space
type PrincStrainsUp struct {

	// constants
	Pert  float64 // perturbation values
	EvTol float64 // tolerance to detect repeated eigenvalues
	Zero  float64 // minimum λ to be considered zero
	Fzero float64 // zero yield function value
	Nsig  int     // number of stress components

	// model
	mdl   EPmodel // elastoplastic model
	nalp  int     // number of α
	nsurf int     // number of yield functions
	fcoef float64 // coefficient to normalise yield function

	// variables
	Lσ      []float64     // eigenvalues of stresses
	Lε      []float64     // eigenvalues of strains
	P       [][]float64   // eigenprojectors of strains and stresses
	εetr    []float64     // trial elastic state
	αn      []float64     // α at beginning of update
	h       []float64     // [nalp] principal values: hardening
	A       []float64     // ∂f/∂α_i       [nalp]
	N       []float64     // ∂f/∂σ         [3]
	Ne      []float64     // ∂f/∂σ・De     [3]
	Nb      []float64     // ∂g/∂σ         [3]
	Mb      [][]float64   // ∂Nb/∂εe       [3][3]
	Mbe     [][]float64   // ∂Nb/∂σ・De    [3][3]
	De      [][]float64   // De = ∂σ/∂εe   [3][3]
	Dt      [][]float64   // Dt = ∂σ/∂εetr [3][3]
	a       [][]float64   // ∂Nb/∂α_i      [nalp][3]
	b       [][]float64   // ∂h_i/∂εe      [nalp][3]
	be      [][]float64   // ∂h_i/∂σ・De   [nalp][3]
	c       [][]float64   // ∂h_i/∂α_j     [nalp][nalp]
	x       []float64     // {εe0, εe1, εe2, α0, α1, ..., Δγ}
	J       [][]float64   // Jacobian      [4+nalp][4+nalp]
	Ji      [][]float64   // inverse of J  [4+nalp][4+nalp]
	dPdεetr [][][]float64 // ∂P_k/∂εetr    [3][nsig][nsig]

	// nonlinear solver
	nls num.NlSolver // nonlinear solver
}

// Init initialises this structure
func (o *PrincStrainsUp) Init(ndim int, prms fun.Prms, mdl EPmodel) (err error) {

	// constants
	o.Pert = 1e-7
	o.EvTol = tsr.EV_EVTOL
	o.Zero = tsr.EV_ZERO
	o.Fzero = 1e-9
	o.Nsig = 2 * ndim

	// model
	o.mdl = mdl
	o.nalp, o.nsurf, o.fcoef, _, _ = o.mdl.Info()

	// variables
	o.Lσ = make([]float64, 3)
	o.Lε = make([]float64, 3)
	o.P = la.MatAlloc(3, o.Nsig)
	o.εetr = make([]float64, o.Nsig)
	o.αn = make([]float64, o.nalp)
	o.h = make([]float64, 3)
	o.A = make([]float64, o.nalp)
	o.N = make([]float64, 3)
	o.Ne = make([]float64, 3)
	o.Nb = make([]float64, 3)
	o.Mb = la.MatAlloc(3, 3)
	o.Mbe = la.MatAlloc(3, 3)
	o.De = la.MatAlloc(3, 3)
	o.Dt = la.MatAlloc(3, 3)
	o.a = la.MatAlloc(o.nalp, 3)
	o.b = la.MatAlloc(o.nalp, 3)
	o.be = la.MatAlloc(o.nalp, 3)
	o.c = la.MatAlloc(o.nalp, o.nalp)
	o.x = make([]float64, 4+o.nalp)
	o.J = la.MatAlloc(4+o.nalp, 4+o.nalp)
	o.Ji = la.MatAlloc(4+o.nalp, 4+o.nalp)
	o.dPdεetr = utl.Deep3alloc(3, o.Nsig, o.Nsig)

	// nonlinear solver
	useDn, numJ := true, false
	o.nls.Init(4+o.nalp, o.ffcn, nil, o.JfcnD, useDn, numJ, map[string]float64{})
	return
}

// Update updates state
func (o PrincStrainsUp) Update(s *State, ε, Δε []float64) (err error) {

	// trial strains and stresses
	o.mdl.ElastUpdate(s, ε, Δε) // also updates Phi

	// check loading condition
	ftr := o.mdl.YieldFuncs(s)[0]
	if ftr <= o.Fzero {
		s.Dgam = 0
		s.Loading = false
		return
	}

	// eigenvalues/projectors
	copy(o.εetr, s.Phi)
	_, err = tsr.M_FixZeroOrRepeated(o.Lε, o.εetr, o.Pert, o.EvTol, o.Zero)
	if err != nil {
		return
	}
	err = tsr.M_EigenValsProjsNum(o.P, o.Lε, o.εetr)
	if err != nil {
		return
	}

	// trial values
	for i := 0; i < 3; i++ {
		o.x[i] = o.Lε[i]
	}
	for i := 0; i < o.nalp; i++ {
		o.αn[i] = s.Alp[i]
		o.x[3+i] = s.Alp[i]
	}
	o.x[3+o.nalp] = 0 // Δγ

	// check Jacobian
	check := false
	tolChk := 1e-5
	silentChk := false
	if check {
		var cnd float64
		cnd, err = o.nls.CheckJ(o.x, tolChk, true, silentChk)
		io.Pfred("before: cnd(J) = %v\n", cnd)
	}

	// solve
	silent := true
	err = o.nls.Solve(o.x, silent)
	if err != nil {
		return
	}

	// check Jacobian again
	if check {
		var cnd float64
		cnd, err = o.nls.CheckJ(o.x, tolChk, true, silentChk)
		io.Pfred("after: cnd(J) = %v\n", cnd)
		if err != nil {
			return
		}
	}

	// set new state
	εe, α, Δγ := o.x[:3], o.x[3:3+o.nalp], o.x[3+o.nalp]
	o.mdl.E_CalcSig(o.Lσ, εe)
	for i := 0; i < o.Nsig; i++ {
		s.Sig[i] = o.Lσ[0]*o.P[0][i] + o.Lσ[1]*o.P[1][i] + o.Lσ[2]*o.P[2][i]
		s.Phi[i] = εe[0]*o.P[0][i] + εe[1]*o.P[1][i] + εe[2]*o.P[2][i]
	}
	copy(s.Alp, α)
	s.Dgam = Δγ
	s.Loading = true
	return
}

// CalcD computes algorithm tangent operator
func (o PrincStrainsUp) CalcD(D [][]float64, s *State) (err error) {

	// elastic response
	if !s.Loading {
		o.mdl.ElastD(D, s)
		return
	}

	// eigenvalues/projectors
	copy(o.εetr, s.Phi)
	_, err = tsr.M_FixZeroOrRepeated(o.Lε, o.εetr, o.Pert, o.EvTol, o.Zero)
	if err != nil {
		return
	}
	err = tsr.M_EigenValsProjsNum(o.P, o.Lε, o.εetr)
	if err != nil {
		return
	}

	// derivatives of eigenprojectors w.r.t trial elastic strains
	err = tsr.M_EigenProjsDeriv(o.dPdεetr, o.εetr, o.Lε, o.P, o.Zero)
	if err != nil {
		return
	}

	// eigenvalues of stress
	err = tsr.M_EigenValsNum(o.Lσ, s.Sig)
	if err != nil {
		return
	}

	// x vector
	for i := 0; i < 3; i++ {
		o.x[i] = o.Lε[i]
	}
	for i := 0; i < o.nalp; i++ {
		o.x[3+i] = s.Alp[i]
	}
	o.x[3+o.nalp] = s.Dgam

	// compute J and Ji
	err = o.JfcnD(o.J, o.x)
	if err != nil {
		return
	}
	err = la.MatInvG(o.Ji, o.J, 1e-10)
	if err != nil {
		return
	}

	// compute De and Dt = De * Ji
	o.mdl.E_CalcDe(o.De, o.Lε)
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			o.Dt[i][j] = 0
			for k := 0; k < 3; k++ {
				o.Dt[i][j] += o.De[i][k] * o.Ji[k][j]
			}
		}
	}

	// compute D
	for i := 0; i < o.Nsig; i++ {
		for j := 0; j < o.Nsig; j++ {
			D[i][j] = 0.0
			for k := 0; k < 3; k++ {
				for l := 0; l < 3; l++ {
					D[i][j] += o.Dt[k][l] * o.P[k][i] * o.P[l][j]
				}
				D[i][j] += o.Lσ[k] * o.dPdεetr[k][i][j]
			}
		}
	}
	return
}

// ffcn is the nonlinear solver function
func (o PrincStrainsUp) ffcn(fx, x []float64) error {
	εe, α, Δγ := x[:3], x[3:3+o.nalp], x[3+o.nalp]
	εetr := o.Lε
	o.mdl.E_CalcSig(o.Lσ, εe)
	f, err := o.mdl.L_FlowHard(o.Nb, o.h, o.Lσ, α)
	if err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		fx[i] = εe[i] - εetr[i] + Δγ*o.Nb[i]
	}
	for i := 0; i < o.nalp; i++ {
		fx[3+i] = α[i] - o.αn[i] - Δγ*o.h[i]
	}
	fx[3+o.nalp] = f / o.fcoef
	return nil
}

// JfcnD is the nonlinear solver Jacobian: J = dfdx
func (o PrincStrainsUp) JfcnD(J [][]float64, x []float64) error {
	εe, α, Δγ := x[:3], x[3:3+o.nalp], x[3+o.nalp]
	o.mdl.E_CalcSig(o.Lσ, εe)
	err := o.mdl.L_SecondDerivs(o.N, o.Nb, o.A, o.h, o.Mb, o.a, o.b, o.c, o.Lσ, α)
	if err != nil {
		return err
	}
	o.mdl.E_CalcDe(o.De, εe)
	for i := 0; i < 3; i++ {
		o.Ne[i] = 0
		for m := 0; m < o.nalp; m++ {
			o.be[m][i] = 0
		}
		for j := 0; j < 3; j++ {
			o.Ne[i] += o.N[j] * o.De[j][i]
			for m := 0; m < o.nalp; m++ {
				o.be[m][i] += o.b[m][j] * o.De[j][i]
			}
			o.Mbe[i][j] = 0
			for k := 0; k < 3; k++ {
				o.Mbe[i][j] += o.Mb[i][k] * o.De[k][j]
			}
		}
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			J[i][j] = tsr.IIm[i][j] + Δγ*o.Mbe[i][j]
		}
		for j := 0; j < o.nalp; j++ {
			J[i][3+j] = Δγ * o.a[j][i]
			J[3+j][i] = -Δγ * o.be[j][i]
		}
		J[i][3+o.nalp] = o.Nb[i]
		J[3+o.nalp][i] = o.Ne[i] / o.fcoef
	}
	for i := 0; i < o.nalp; i++ {
		for j := 0; j < o.nalp; j++ {
			J[3+i][3+j] = tsr.IIm[i][j] - Δγ*o.c[i][j]
		}
		J[3+i][3+o.nalp] = -o.h[i]
		J[3+o.nalp][3+i] = o.A[i] / o.fcoef
	}
	return nil
}
