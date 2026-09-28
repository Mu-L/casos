import React, {useState} from "react";
import i18next from "i18next";
import {Cloud, Link2, Plus, Radio} from "lucide-react";
import * as MeshBackend from "@/backend/MeshBackend";
import {runAction, useResource} from "@/hooks/use-resource";
import {Badge} from "@/components/ui/badge";
import {Button} from "@/components/ui/button";
import {Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle} from "@/components/ui/card";
import {Input} from "@/components/ui/input";
import {Field, FormDialog} from "@/components/shared/form-dialog";
import {NumberInput} from "@/components/shared/number-input";
import {CodeBlock, CodeText} from "@/components/shared/misc";

// Switching role restarts CasOS; the page waits this long before reloading so
// it does not catch the old process on its way out.
const RESTART_RELOAD_DELAY = 8000;

function reloadAfterRestart() {
  setTimeout(() => window.location.reload(), RESTART_RELOAD_DELAY);
}

/**
 * The machine's place in a multi-machine cloud: a standalone CasOS can become
 * a hub or join one, and a hub hands out invites for other computers.
 */
export function CloudCard({onChanged}) {
  const {data: status, loading} = useResource(() => MeshBackend.getMeshStatus(), [], {initialData: null});
  const [dialog, setDialog] = useState(null);

  if (loading || !status || status.role === "member") {
    return null;
  }

  const isHub = status.role === "hub";
  return (
    <Card className="gap-4" data-testid="cloud-card">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Cloud className="text-muted-foreground size-4" />
          {i18next.t("machine:Cloud")}
          {isHub ? <Badge variant="success">{i18next.t("machine:Hub")}</Badge> : null}
        </CardTitle>
        <CardDescription>
          {isHub ? i18next.t("machine:Cloud hub - Description") : i18next.t("machine:Cloud standalone - Description")}
        </CardDescription>
        <CardAction className="flex gap-2">
          {isHub ? (
            <Button size="sm" onClick={() => setDialog("invite")}>
              <Plus />
              {i18next.t("machine:Invite a computer")}
            </Button>
          ) : (
            <>
              <Button variant="outline" size="sm" onClick={() => setDialog("join")}>
                <Link2 />
                {i18next.t("machine:Join a cloud")}
              </Button>
              <Button size="sm" onClick={() => setDialog("hub")}>
                <Radio />
                {i18next.t("machine:Make this a cloud hub")}
              </Button>
            </>
          )}
        </CardAction>
      </CardHeader>
      {isHub ? (
        <CardContent className="text-muted-foreground flex flex-wrap gap-x-8 gap-y-2 text-sm">
          <span>
            {i18next.t("machine:Join address")}: <CodeText copyable>{status.publicUrl}</CodeText>
          </span>
          <span>
            {i18next.t("machine:Overlay address")}: <CodeText>{status.overlayIp}</CodeText>
          </span>
          <span>
            {i18next.t("machine:Members online")}: {status.onlineMembers?.length ?? 0}
          </span>
        </CardContent>
      ) : null}

      <InviteDialog open={dialog === "invite"} onOpenChange={(open) => !open && setDialog(null)} onCreated={onChanged} />
      <EnableHubDialog open={dialog === "hub"} onOpenChange={(open) => !open && setDialog(null)} />
      <JoinCloudDialog open={dialog === "join"} onOpenChange={(open) => !open && setDialog(null)} />
    </Card>
  );
}

function InviteDialog({open, onOpenChange, onCreated}) {
  const [hours, setHours] = useState(24);
  const [uses, setUses] = useState(1);
  const [invite, setInvite] = useState(null);
  const [submitting, setSubmitting] = useState(false);

  function close(next) {
    if (!next) {
      setInvite(null);
    }
    onOpenChange(next);
  }

  async function create() {
    setSubmitting(true);
    await runAction(MeshBackend.addMeshInvite(hours, uses), {
      onSuccess: (res) => {
        setInvite(res.data);
        onCreated?.();
      },
    });
    setSubmitting(false);
  }

  if (invite) {
    return (
      <FormDialog
        open={open}
        onOpenChange={close}
        title={i18next.t("machine:Invite a computer")}
        description={i18next.t("machine:Invite created - Description")}
        footer={<Button onClick={() => close(false)}>{i18next.t("general:OK")}</Button>}
      >
        <Field label={i18next.t("machine:Run this on the computer")}>
          <CodeBlock copyable className="break-all whitespace-pre-wrap">
            {invite.command}
          </CodeBlock>
        </Field>
        <Field label={i18next.t("machine:Or paste into Join a cloud")} hint={i18next.t("machine:Invite shown once")}>
          <div className="grid gap-2 text-sm">
            <CodeText copyable>{invite.hubUrl}</CodeText>
            <CodeText copyable>{invite.token}</CodeText>
          </div>
        </Field>
      </FormDialog>
    );
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={close}
      title={i18next.t("machine:Invite a computer")}
      description={i18next.t("machine:Invite - Description")}
      submitText={i18next.t("machine:Create invite")}
      submitting={submitting}
      onSubmit={create}
    >
      <div className="grid grid-cols-2 gap-3">
        <Field label={i18next.t("machine:Valid for (hours)")}>
          <NumberInput value={hours} onChange={setHours} min={1} max={720} />
        </Field>
        <Field label={i18next.t("machine:Computers it can add")}>
          <NumberInput value={uses} onChange={setUses} min={1} max={100} />
        </Field>
      </div>
    </FormDialog>
  );
}

function EnableHubDialog({open, onOpenChange}) {
  const [address, setAddress] = useState("");
  const [submitting, setSubmitting] = useState(false);

  async function enable() {
    setSubmitting(true);
    const ok = await runAction(MeshBackend.enableMeshHub(address), {
      successMessage: i18next.t("machine:CasOS is restarting"),
    });
    if (ok) {
      reloadAfterRestart();
      return;
    }
    setSubmitting(false);
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={i18next.t("machine:Make this a cloud hub")}
      description={i18next.t("machine:Make hub - Description")}
      submitText={i18next.t("machine:Turn on and restart")}
      submitting={submitting}
      submitDisabled={!address.trim()}
      onSubmit={enable}
    >
      <Field
        label={i18next.t("machine:Public address")}
        htmlFor="mesh-public-address"
        required
        hint={i18next.t("machine:Public address - Hint")}
      >
        <Input
          id="mesh-public-address"
          value={address}
          onChange={(event) => setAddress(event.target.value)}
          placeholder="203.0.113.10"
          className="font-mono text-xs"
        />
      </Field>
    </FormDialog>
  );
}

function JoinCloudDialog({open, onOpenChange}) {
  const [hubUrl, setHubUrl] = useState("");
  const [token, setToken] = useState("");
  const [submitting, setSubmitting] = useState(false);

  // The command a hub hands out pastes whole into either field.
  function acceptPaste(value) {
    const parts = value.trim().split(/\s+/);
    if (parts.length === 4 && parts[0] === "casos" && parts[1] === "join") {
      setHubUrl(parts[2]);
      setToken(parts[3]);
      return true;
    }
    return false;
  }

  async function join() {
    setSubmitting(true);
    const ok = await runAction(MeshBackend.joinMesh(hubUrl.trim(), token.trim()), {
      successMessage: i18next.t("machine:CasOS is restarting"),
    });
    if (ok) {
      reloadAfterRestart();
      return;
    }
    setSubmitting(false);
  }

  return (
    <FormDialog
      open={open}
      onOpenChange={onOpenChange}
      title={i18next.t("machine:Join a cloud")}
      description={i18next.t("machine:Join - Description")}
      submitText={i18next.t("machine:Join and restart")}
      submitting={submitting}
      submitDisabled={!hubUrl.trim() || !token.trim()}
      onSubmit={join}
    >
      <Field label={i18next.t("machine:Cloud address")} htmlFor="mesh-hub-url" required>
        <Input
          id="mesh-hub-url"
          value={hubUrl}
          onChange={(event) => acceptPaste(event.target.value) || setHubUrl(event.target.value)}
          placeholder="https://203.0.113.10:20444"
          className="font-mono text-xs"
        />
      </Field>
      <Field label={i18next.t("machine:Invite")} htmlFor="mesh-token" required>
        <Input
          id="mesh-token"
          value={token}
          onChange={(event) => acceptPaste(event.target.value) || setToken(event.target.value)}
          placeholder="casos1.…"
          className="font-mono text-xs"
        />
      </Field>
    </FormDialog>
  );
}
