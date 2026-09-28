package controllers

import (
	"encoding/json"

	"github.com/casosorg/casos/deploy"
	"github.com/casosorg/casos/object"
)

func (c *ApiController) GetGlobalMachines() {
	if c.RequireAdmin() {
		return
	}

	machines, err := object.GetGlobalMachines()
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(machines)
}

func (c *ApiController) GetMachine() {
	if c.RequireAdmin() {
		return
	}

	id := c.Input().Get("id")

	machine, err := object.GetMachine(id)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(machine)
}

func (c *ApiController) UpdateMachine() {
	if c.RequireAdmin() {
		return
	}

	id := c.Input().Get("id")

	var machine object.Machine
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	success, err := object.UpdateMachine(id, &machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(success)
}

func (c *ApiController) AddMachine() {
	if c.RequireAdmin() {
		return
	}

	var machine object.Machine
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	success, err := object.AddMachine(&machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(success)
}

func (c *ApiController) DeleteMachine() {
	if c.RequireAdmin() {
		return
	}

	var machine object.Machine
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	// A machine that joined over the internet also holds a node, a place on
	// the overlay and a credential, which go with it.
	if existing, err := object.GetMachine(machine.Owner + "/" + machine.Name); err == nil && existing != nil && existing.AuthType == object.MachineAuthTypeAgent {
		if err := deploy.RemoveMeshMember(c.Ctx.Request.Context(), existing.Owner, existing.Name); err != nil {
			c.ResponseError(err.Error())
			return
		}
		c.ResponseOk(true)
		return
	}

	success, err := object.DeleteMachine(&machine)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(success)
}
