import * as Setting from "../Setting";

function get(path) {
  return fetch(`${Setting.ServerUrl}${path}`, {
    method: "GET",
    credentials: "include",
    headers: {"Accept-Language": Setting.getAcceptLanguage()},
  }).then(res => Setting.handleFetchResponse(res));
}

function post(path, body) {
  return fetch(`${Setting.ServerUrl}${path}`, {
    method: "POST",
    credentials: "include",
    headers: {"Accept-Language": Setting.getAcceptLanguage()},
    body: JSON.stringify(body ?? {}),
  }).then(res => Setting.handleFetchResponse(res));
}

export function getMeshStatus() {
  return get("/api/get-mesh-status");
}

export function addMeshInvite(hours, uses) {
  return post("/api/add-mesh-invite", {hours, uses});
}

export function getMeshInvites() {
  return get("/api/get-mesh-invites");
}

export function deleteMeshInvite(name) {
  return post("/api/delete-mesh-invite", {name});
}

export function enableMeshHub(publicAddress) {
  return post("/api/enable-mesh-hub", {publicAddress});
}

export function joinMesh(hubUrl, token) {
  return post("/api/join-mesh", {hubUrl, token});
}

export function leaveMesh(force) {
  return post("/api/leave-mesh", {force});
}
