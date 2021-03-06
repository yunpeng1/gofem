// Copyright 2016 The Gofem Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// package solid implements elements for the solution of solid mechanics problems
package solid

import (
	"github.com/cpmech/gofem/ele"
	"github.com/cpmech/gofem/inp"
	"github.com/cpmech/gofem/mdl/solid"
	"github.com/cpmech/gofem/shp"

	"github.com/cpmech/gosl/chk"
	"github.com/cpmech/gosl/fun"
	"github.com/cpmech/gosl/io"
	"github.com/cpmech/gosl/la"
	"github.com/cpmech/gosl/tsr"
	"github.com/cpmech/gosl/utl"
)

// Solid represents a solid element with displacements u as primary variables
type Solid struct {

	// basic data
	Cell *inp.Cell   // the cell structure
	X    [][]float64 // matrix of nodal coordinates [ndim][nnode]
	Nu   int         // total number of unknowns
	Ndim int         // space dimension

	// variables for dynamics
	Cdam float64  // coefficient for damping // TODO: read this value
	Gfcn fun.Func // gravity function

	// optional data
	UseB      bool    // use B matrix
	Thickness float64 // thickness
	Debug     bool    // debugging flag

	// integration points
	IpsElem []shp.Ipoint // integration points of element
	IpsFace []shp.Ipoint // integration points corresponding to faces

	// material model and internal variables
	Mdl      solid.Model // material model
	MdlSmall solid.Small // model specialisation for small strains
	MdlLarge solid.Large // model specialisation for large deformations

	// internal variables
	States    []*solid.State // [nip] states
	StatesBkp []*solid.State // [nip] backup states
	StatesAux []*solid.State // [nip] auxiliary backup states

	// additional variables
	Umap   []int            // assembly map (location array/element equations)
	NatBcs []*ele.NaturalBc // natural boundary conditions
	Emat   [][]float64      // [nvert][nip] extrapolator matrix; if AddToExt is called

	// local starred variables
	Zet    [][]float64 // [nip][ndim] t2 star vars: ζ* = α1.u + α2.v + α3.a
	Chi    [][]float64 // [nip][ndim] t2 star vars: χ* = α4.u + α5.v + α6.a
	DivChi []float64   // [nip] divergent of χs (for coupled sims)

	// scratchpad. computed @ each ip
	Grav []float64   // [ndim] gravity vector
	Us   []float64   // [ndim] displacements @ ip
	Fi   []float64   // [nu] internal forces
	K    [][]float64 // [nu][nu] consistent tangent (stiffness) matrix
	B    [][]float64 // [nsig][nu] B matrix for axisymetric case
	D    [][]float64 // [nsig][nsig] constitutive consistent tangent matrix

	// strains
	Eps    []float64 // total (updated) strains
	DelEps []float64 // incremental strains leading to updated strains

	// debugging
	Fex []float64 // x-components of external surface forces
	Fey []float64 // y-components of external syrface forces
	Fez []float64 // z-components of external syrface forces

	// contact (see e_u_contact.go)
	Nq            int         // number of qb variables
	HasContact    bool        // indicates if this element has contact faces
	Vid2contactId []int       // [nverts] maps local vertex id to index in Qmap
	ContactId2vid []int       // [nq] maps contact face variable id to local vertex id
	Qmap          []int       // [nq] map of "qb" variables (contact face)
	Macaulay      bool        // contact: use discrete ramp function instead of smooth ramp
	BetRmp        float64     // contact: coefficient for Sramp
	Kap           float64     // contact: κ coefficient to normalise equation for contact face modelling
	Kuq           [][]float64 // [nu][nq] Kuq := dRu/dq consistent tangent matrix
	Kqu           [][]float64 // [nq][nu] Kqu := dRq/du consistent tangent matrix
	Kqq           [][]float64 // [nq][nq] Kqq := dRq/dq consistent tangent matrix

	// XFEM (material interface or not)
	Xmat bool        // material interface
	Xcrk bool        // crack
	Xfem bool        // Xmat || Xcrk
	Na   int         // number of additional degrees of freedom (XFEM)
	Amap []int       // additional DOFs map
	Kua  [][]float64 // TODO: [nu][na] Kua := dRu/da consistent tangent matrix
	Kau  [][]float64 // TODO: [na][nu] Kau := dRa/du consistent tangent matrix
	Kaa  [][]float64 // TODO: [na][na] Kaa := dRa/da consistent tangent matrix
	//ProxyMesh *inp.Mesh      // TODO: auxiliary mesh
	//EnrichShp *shp.EnrichShp // TODO: enriched shape functions
}

// initialisation ///////////////////////////////////////////////////////////////////////////////////

// register element
func init() {

	// information allocator
	ele.SetInfoFunc("solid", func(sim *inp.Simulation, cell *inp.Cell, edat *inp.ElemData) *ele.Info {

		// number of nodes in element
		nverts := cell.Shp.Nverts
		if nverts < 0 {
			return nil // fail
		}

		// set DOFS and other information
		var info ele.Info
		ykeys := []string{"ux", "uy"}
		if sim.Ndim == 3 {
			ykeys = []string{"ux", "uy", "uz"}
		}
		info.Dofs = make([][]string, nverts)
		for m := 0; m < nverts; m++ {
			info.Dofs[m] = ykeys
		}
		info.Y2F = map[string]string{"ux": "fx", "uy": "fy", "uz": "fz"}
		info.T2vars = ykeys

		// contact: extra information
		contact_set_info(&info, cell, edat)

		// xfem: extra information
		xfem_set_info(&info, cell, edat)

		// number of internal values to be extrapolated
		if cell.Extrap {
			info.Nextrap = 2 * sim.Ndim // nsig
		}

		// results
		return &info
	})

	// element allocator
	ele.SetAllocator("solid", func(sim *inp.Simulation, cell *inp.Cell, edat *inp.ElemData, x [][]float64) ele.Element {

		// basic data
		var o Solid
		o.Cell = cell
		o.X = x
		o.Ndim = len(x)
		o.Nu = o.Ndim * o.Cell.Shp.Nverts

		// parse flags
		o.UseB, o.Debug, o.Thickness = GetSolidFlags(sim.Data.Axisym, sim.Data.Pstress, edat.Extra)

		// integration points
		var err error
		o.IpsElem, o.IpsFace, err = o.Cell.Shp.GetIps(edat.Nip, edat.Nipf)
		if err != nil {
			chk.Panic("cannot allocate integration points of solid element with nip=%d and nipf=%d:\n%v", edat.Nip, edat.Nipf, err)
		}
		nip := len(o.IpsElem)

		// model
		mat := sim.MatModels.Get(edat.Mat)
		if mat == nil {
			chk.Panic("cannot find material %q for solid element {tag=%d, id=%d}\n", edat.Mat, cell.Tag, cell.Id)
		}
		o.Mdl = mat.Sld

		// model specialisations
		switch m := o.Mdl.(type) {
		case solid.Small:
			o.MdlSmall = m
		case solid.Large:
			o.MdlLarge = m
		default:
			chk.Panic("__internal_error__: 'u' element cannot determine the type of the material model")
		}

		// local starred variables
		o.Zet = la.MatAlloc(nip, o.Ndim)
		o.Chi = la.MatAlloc(nip, o.Ndim)
		o.DivChi = make([]float64, nip)

		// scratchpad. computed @ each ip
		nsig := 2 * o.Ndim
		o.Grav = make([]float64, o.Ndim)
		o.Us = make([]float64, o.Ndim)
		o.Fi = make([]float64, o.Nu)
		o.D = la.MatAlloc(nsig, nsig)
		o.K = la.MatAlloc(o.Nu, o.Nu)
		if o.UseB {
			o.B = la.MatAlloc(nsig, o.Nu)
		}

		// strains
		o.Eps = make([]float64, nsig)
		o.DelEps = make([]float64, nsig)

		// variables for debugging
		if o.Debug {
			o.Fex = make([]float64, o.Cell.Shp.Nverts)
			o.Fey = make([]float64, o.Cell.Shp.Nverts)
			if o.Ndim == 3 {
				o.Fez = make([]float64, o.Cell.Shp.Nverts)
			}
		}

		// surface loads (natural boundary conditions)
		for _, fc := range cell.FaceBcs {
			o.NatBcs = append(o.NatBcs, &ele.NaturalBc{fc.Cond, fc.FaceId, fc.Func, fc.Extra})
		}

		// contact: init
		o.contact_init(edat)

		// xfem: init
		o.xfem_init(edat)

		// return new element
		return &o
	})
}

// implementation ///////////////////////////////////////////////////////////////////////////////////

// Id returns the cell Id
func (o *Solid) Id() int { return o.Cell.Id }

// SetEqs set equations
func (o *Solid) SetEqs(eqs [][]int, mixedform_eqs []int) (err error) {

	// standard DOFs
	o.Umap = make([]int, o.Nu)
	for m := 0; m < o.Cell.Shp.Nverts; m++ {
		for i := 0; i < o.Ndim; i++ {
			r := i + m*o.Ndim
			o.Umap[r] = eqs[m][i]
		}
	}

	// contact DOFs
	ndn := o.Ndim // number of degrees of freedom per node set already
	if o.HasContact {
		for i, m := range o.ContactId2vid {
			o.Qmap[i] = eqs[m][o.Ndim]
		}
		ndn += 1 // TODO: check this
	}

	// xfem DOFs
	if o.Xfem {
		for m := 0; m < o.Cell.Shp.Nverts; m++ {
			for i := 0; i < o.Na; i++ {
				r := i + m*o.Na
				o.Amap[r] = eqs[m][ndn+i]
			}
		}
	}
	return
}

// SetEleConds set element conditions
func (o *Solid) SetEleConds(key string, f fun.Func, extra string) (err error) {
	if key == "g" { // gravity
		o.Gfcn = f
	}
	return
}

// InterpStarVars interpolates star variables to integration points
func (o *Solid) InterpStarVars(sol *ele.Solution) (err error) {

	// for each integration point
	for idx, ip := range o.IpsElem {

		// interpolation functions and gradients
		err = o.Cell.Shp.CalcAtIp(o.X, ip, true)
		if err != nil {
			return
		}
		S := o.Cell.Shp.S
		G := o.Cell.Shp.G

		// interpolate starred variables
		o.DivChi[idx] = 0
		for i := 0; i < o.Ndim; i++ {
			o.Zet[idx][i] = 0
			o.Chi[idx][i] = 0
			for m := 0; m < o.Cell.Shp.Nverts; m++ {
				r := o.Umap[i+m*o.Ndim]
				o.Zet[idx][i] += S[m] * sol.Zet[r]
				o.Chi[idx][i] += S[m] * sol.Chi[r]
				o.DivChi[idx] += G[m][i] * sol.Chi[r]
			}
		}
	}
	return
}

// AddToRhs adds -R to global residual vector fb
func (o *Solid) AddToRhs(fb []float64, sol *ele.Solution) (err error) {

	// clear Fi vector if using B matrix
	if o.UseB {
		la.VecFill(o.Fi, 0)
	}

	// compute gravity
	if o.Gfcn != nil {
		o.Grav[o.Ndim-1] = -o.Gfcn.F(sol.T, nil)
	}

	// for each integration point
	ρ := o.Mdl.GetRho()
	nverts := o.Cell.Shp.Nverts
	for idx, ip := range o.IpsElem {

		// interpolation functions, gradients and variables @ ip
		err = o.ipvars(idx, sol)
		if err != nil {
			return
		}

		// auxiliary
		coef := o.Cell.Shp.J * ip[3] * o.Thickness
		S := o.Cell.Shp.S
		G := o.Cell.Shp.G

		// add internal forces to fb
		if o.UseB {
			radius := 1.0
			if sol.Axisym {
				radius = o.Cell.Shp.AxisymGetRadius(o.X)
				coef *= radius
			}
			IpBmatrix(o.B, o.Ndim, nverts, G, radius, S, sol.Axisym)
			la.MatTrVecMulAdd(o.Fi, coef, o.B, o.States[idx].Sig) // Fi += coef * tr(B) * σ
		} else {
			for m := 0; m < nverts; m++ {
				for i := 0; i < o.Ndim; i++ {
					r := o.Umap[i+m*o.Ndim]
					for j := 0; j < o.Ndim; j++ {
						fb[r] -= coef * tsr.M2T(o.States[idx].Sig, i, j) * G[m][j] // -Fi
					}
				}
			}
		}

		// dynamic term
		if sol.Steady {
			if o.Gfcn != nil {
				for m := 0; m < nverts; m++ {
					i := o.Ndim - 1
					r := o.Umap[i+m*o.Ndim]
					fb[r] += coef * S[m] * ρ * o.Grav[i] // +fx
				}
			}
		} else {
			α1 := sol.DynCfs.GetAlp1()
			α4 := sol.DynCfs.GetAlp4()
			for m := 0; m < nverts; m++ {
				for i := 0; i < o.Ndim; i++ {
					r := o.Umap[i+m*o.Ndim]
					fb[r] -= coef * S[m] * (ρ*(α1*o.Us[i]-o.Zet[idx][i]-o.Grav[i]) + o.Cdam*(α4*o.Us[i]-o.Chi[idx][i])) // -RuBar
				}
			}
		}
	}

	// assemble fb if using B matrix
	if o.UseB {
		for i, I := range o.Umap {
			fb[I] -= o.Fi[i]
		}
	}

	// external forces
	err = o.AddSurfLoadsToRhs(fb, sol)
	if err != nil {
		return
	}

	// contact: additional term to fb
	err = o.contact_add_to_rhs(fb, sol)

	// xfem: additional term to fb
	err = o.xfem_add_to_rhs(fb, sol)
	return
}

// AddToKb adds element K to global Jacobian matrix Kb
func (o *Solid) AddToKb(Kb *la.Triplet, sol *ele.Solution, firstIt bool) (err error) {

	// zero K matrix
	la.MatFill(o.K, 0)

	// for each integration point
	ρ := o.Mdl.GetRho()
	nverts := o.Cell.Shp.Nverts
	for idx, ip := range o.IpsElem {

		// interpolation functions, gradients and variables @ ip
		err = o.ipvars(idx, sol)
		if err != nil {
			return
		}

		// check Jacobian
		if o.Cell.Shp.J < 0 {
			return chk.Err("Solid: eid=%d: Jacobian is negative = %g\n", o.Id(), o.Cell.Shp.J)
		}

		// auxiliary
		coef := o.Cell.Shp.J * ip[3] * o.Thickness
		S := o.Cell.Shp.S
		G := o.Cell.Shp.G

		// consistent tangent model matrix
		err = o.MdlSmall.CalcD(o.D, o.States[idx], firstIt)
		if err != nil {
			return
		}

		// add contribution to consistent tangent matrix
		if o.UseB {
			radius := 1.0
			if sol.Axisym {
				radius = o.Cell.Shp.AxisymGetRadius(o.X)
				coef *= radius
			}
			IpBmatrix(o.B, o.Ndim, nverts, G, radius, S, sol.Axisym)
			la.MatTrMulAdd3(o.K, coef, o.B, o.D, o.B) // K += coef * tr(B) * D * B
		} else {
			IpAddToKt(o.K, nverts, o.Ndim, coef, G, o.D)
		}

		// dynamic term
		if !sol.Steady {
			α1 := sol.DynCfs.GetAlp1()
			α4 := sol.DynCfs.GetAlp4()
			for m := 0; m < nverts; m++ {
				for i := 0; i < o.Ndim; i++ {
					r := i + m*o.Ndim
					for n := 0; n < nverts; n++ {
						c := i + n*o.Ndim
						o.K[r][c] += coef * S[m] * S[n] * (ρ*α1 + o.Cdam*α4)
					}
				}
			}
		}
	}

	// add Ks to sparse matrix Kb
	switch {

	case o.HasContact:
		err = o.contact_add_to_jac(Kb, sol)

	case o.Xfem:
		err = o.xfem_add_to_jac(Kb, sol)

	default:
		for i, I := range o.Umap {
			for j, J := range o.Umap {
				Kb.Put(I, J, o.K[i][j])
			}
		}
	}
	return
}

// Update perform (tangent) update
func (o *Solid) Update(sol *ele.Solution) (err error) {

	// for each integration point
	nverts := o.Cell.Shp.Nverts
	for idx, ip := range o.IpsElem {

		// interpolation functions and gradients
		err = o.Cell.Shp.CalcAtIp(o.X, ip, true)
		if err != nil {
			return
		}
		S := o.Cell.Shp.S
		G := o.Cell.Shp.G

		// compute strains
		if o.UseB {
			radius := 1.0
			if sol.Axisym {
				radius = o.Cell.Shp.AxisymGetRadius(o.X)
			}
			IpBmatrix(o.B, o.Ndim, nverts, G, radius, S, sol.Axisym)
			IpStrainsAndIncB(o.Eps, o.DelEps, 2*o.Ndim, o.Nu, o.B, sol.Y, sol.ΔY, o.Umap)
		} else {
			IpStrainsAndInc(o.Eps, o.DelEps, nverts, o.Ndim, sol.Y, sol.ΔY, o.Umap, G)
		}

		// call model update => update stresses
		err = o.MdlSmall.Update(o.States[idx], o.Eps, o.DelEps, o.Id(), idx, sol.T)
		if err != nil {
			return chk.Err("Update failed (eid=%d, ip=%d)\nΔε=%v\n%v", o.Id(), idx, o.DelEps, err)
		}
	}
	return
}

// internal variables ///////////////////////////////////////////////////////////////////////////////

// SetIniIvs sets initial ivs for given values in sol and ivs map
func (o *Solid) SetIniIvs(sol *ele.Solution, ivs map[string][]float64) (err error) {

	// allocate slices of states
	nip := len(o.IpsElem)
	o.States = make([]*solid.State, nip)
	o.StatesBkp = make([]*solid.State, nip)
	o.StatesAux = make([]*solid.State, nip)

	// has specified stresses?
	_, has_sig := ivs["sx"]

	// for each integration point
	σ := make([]float64, 2*o.Ndim)
	for i := 0; i < nip; i++ {
		if has_sig {
			Ivs2sigmas(σ, i, ivs)
		}
		o.States[i], err = o.Mdl.InitIntVars(σ)
		if err != nil {
			return
		}
		o.StatesBkp[i] = o.States[i].GetCopy()
		o.StatesAux[i] = o.States[i].GetCopy()
	}
	return
}

// BackupIvs create copy of internal variables
func (o *Solid) BackupIvs(aux bool) (err error) {
	if aux {
		for i, s := range o.StatesAux {
			s.Set(o.States[i])
		}
		return
	}
	for i, s := range o.StatesBkp {
		s.Set(o.States[i])
	}
	return
}

// RestoreIvs restore internal variables from copies
func (o *Solid) RestoreIvs(aux bool) (err error) {
	if aux {
		for i, s := range o.States {
			s.Set(o.StatesAux[i])
		}
		return
	}
	for i, s := range o.States {
		s.Set(o.StatesBkp[i])
	}
	return
}

// Ureset fixes internal variables after u (displacements) have been zeroed
func (o *Solid) Ureset(sol *ele.Solution) (err error) {
	for idx, _ := range o.IpsElem {
		if len(o.States[idx].F) > 0 {
			la.MatFill(o.States[idx].F, 0)
			la.MatFill(o.StatesBkp[idx].F, 0)
		}
	}
	return
}

// writer ///////////////////////////////////////////////////////////////////////////////////////////

// Encode encodes internal variables
func (o *Solid) Encode(enc utl.Encoder) (err error) {
	return enc.Encode(o.States)
}

// Decode decodes internal variables
func (o *Solid) Decode(dec utl.Decoder) (err error) {
	err = dec.Decode(&o.States)
	if err != nil {
		return
	}
	return o.BackupIvs(false)
}

// OutIpCoords returns the coordinates of integration points
func (o *Solid) OutIpCoords() (C [][]float64) {
	C = make([][]float64, len(o.IpsElem))
	for idx, ip := range o.IpsElem {
		C[idx] = o.Cell.Shp.IpRealCoords(o.X, ip)
	}
	return
}

// OutIpKeys returns the integration points' keys
func (o *Solid) OutIpKeys() []string {
	keys := StressKeys(o.Ndim)
	for i := 0; i < len(o.States[0].Alp); i++ {
		keys = append(keys, io.Sf("alp%d", i))
	}
	return keys
}

// OutIpVals returns the integration points' values corresponding to keys
func (o *Solid) OutIpVals(M *ele.IpsMap, sol *ele.Solution) {
	nip := len(o.IpsElem)
	for i, key := range StressKeys(o.Ndim) {
		for idx, _ := range o.IpsElem {
			M.Set(key, idx, nip, o.States[idx].Sig[i])
		}
	}
	for i := 0; i < len(o.States[0].Alp); i++ {
		key := io.Sf("alp%d", i)
		for idx, _ := range o.IpsElem {
			M.Set(key, idx, nip, o.States[idx].Alp[i])
		}
	}
}

// extra ////////////////////////////////////////////////////////////////////////////////////////////

// AddToExt extrapolates stresses at integration points to nodes
func (o *Solid) AddToExt(sol *ele.Solution) (err error) {
	nverts := o.Cell.Shp.Nverts
	nsig := 2 * o.Ndim
	nip := len(o.IpsElem)
	if len(o.Emat) == 0 {
		o.Emat = la.MatAlloc(nverts, nip)
		err = o.Cell.Shp.Extrapolator(o.Emat, o.IpsElem)
		if err != nil {
			return
		}
	}
	for m := 0; m < nverts; m++ {
		vid := o.Cell.Verts[m]
		sol.Cnt[vid] += 1
		if len(sol.Ext[vid]) == 0 {
			sol.Ext[vid] = make([]float64, nsig)
		}
		for i := 0; i < nsig; i++ {
			for idx, _ := range o.IpsElem {
				σ := o.States[idx].Sig
				sol.Ext[vid][i] += o.Emat[m][idx] * σ[i]
			}
		}
	}
	return
}

// auxiliary ////////////////////////////////////////////////////////////////////////////////////////

// ipvars computes current values @ integration points. idx == index of integration point
func (o *Solid) ipvars(idx int, sol *ele.Solution) (err error) {

	// interpolation functions and gradients
	err = o.Cell.Shp.CalcAtIp(o.X, o.IpsElem[idx], true)
	if err != nil {
		return
	}

	// skip if steady (this must be after CalcAtIp, because callers will need S and G)
	if sol.Steady {
		return
	}

	// clear variables
	for i := 0; i < o.Ndim; i++ {
		o.Us[i] = 0
	}

	// recover u-variables @ ip
	for m := 0; m < o.Cell.Shp.Nverts; m++ {
		for i := 0; i < o.Ndim; i++ {
			r := o.Umap[i+m*o.Ndim]
			o.Us[i] += o.Cell.Shp.S[m] * sol.Y[r]
		}
	}
	return
}

// surfloads_keys returns the keys that can be used to specify surface loads
func (o *Solid) surfloads_keys() map[string]bool {
	return map[string]bool{"qn": true, "qn0": true, "aqn": true}
}

// AddSurfLoadsToRhs adds surfaces loads to rhs
func (o *Solid) AddSurfLoadsToRhs(fb []float64, sol *ele.Solution) (err error) {

	// debugging variables
	if o.Debug {
		la.VecFill(o.Fex, 0)
		la.VecFill(o.Fey, 0)
		if o.Ndim == 3 {
			la.VecFill(o.Fez, 0)
		}
	}

	// compute surface integral
	var res float64
	for _, nbc := range o.NatBcs {

		// function evaluation
		res = nbc.Fcn.F(sol.T, nil)

		// loop over ips of face
		for _, ipf := range o.IpsFace {

			// interpolation functions and gradients @ face
			iface := nbc.IdxFace
			err = o.Cell.Shp.CalcAtFaceIp(o.X, ipf, iface)
			if err != nil {
				return
			}
			Sf := o.Cell.Shp.Sf
			nvec := o.Cell.Shp.Fnvec

			// select natural boundary condition type
			switch nbc.Key {

			// distributed load
			case "qn", "qn0", "aqn":
				coef := ipf[3] * res * o.Thickness
				if sol.Axisym && nbc.Key == "aqn" {
					coef *= o.Cell.Shp.AxisymGetRadiusF(o.X, iface)
				}
				for j, m := range o.Cell.Shp.FaceLocalVerts[iface] {
					for i := 0; i < o.Ndim; i++ {
						r := o.Umap[i+m*o.Ndim]
						fb[r] += coef * Sf[j] * nvec[i] // +fe
					}
					if o.Debug {
						o.Fex[m] += coef * Sf[j] * nvec[0]
						o.Fey[m] += coef * Sf[j] * nvec[1]
						if o.Ndim == 3 {
							o.Fez[m] += coef * Sf[j] * nvec[2]
						}
					}
				}
			}
		}
	}
	return
}

// fipvars computes current values @ face integration points
// computes also displacements (us) @ face
func (o *Solid) fipvars(fidx int, sol *ele.Solution) (qb float64) {
	Sf := o.Cell.Shp.Sf
	for i := 0; i < o.Ndim; i++ {
		o.Us[i] = 0
	}
	for i, m := range o.Cell.Shp.FaceLocalVerts[fidx] {
		μ := o.Vid2contactId[m]
		qb += Sf[i] * sol.Y[o.Qmap[μ]]
		for j := 0; j < o.Ndim; j++ {
			r := j + m*o.Ndim
			o.Us[j] += Sf[i] * sol.Y[o.Umap[r]]
		}
	}
	return
}
