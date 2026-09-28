import {useEffect} from "react";
import {useTranslation} from "react-i18next";
import {useHistory} from "react-router-dom";
import {toast} from "sonner";
import * as HelmBackend from "@/backend/HelmBackend";
import {useUiMode} from "@/hooks/use-ui-mode";
import {claimStoredHelmTask, listStoredHelmInstalls} from "@/lib/helmTaskStorage";

const POLL_INTERVAL = 5000;
const TASK_NOT_FOUND_CODE = "helm_task_not_found";

// Says when an install the dialog handed to the background finishes, on whichever page is open.
export function BackgroundInstallWatcher({enabled}) {
  const {t} = useTranslation();
  const history = useHistory();
  const {resolvePath} = useUiMode();

  useEffect(() => {
    if (!enabled) {
      return undefined;
    }
    let polling = false;
    const action = {label: t("general:Open"), onClick: () => history.push(resolvePath("/helm-releases"))};

    function check() {
      const installs = listStoredHelmInstalls();
      if (polling || installs.length === 0) {
        return;
      }
      polling = true;
      Promise.all(
        installs.map((stored) =>
          HelmBackend.getHelmOperationTask(stored.taskId)
            .then((res) => {
              if (res.status !== "ok") {
                if (res.data === TASK_NOT_FOUND_CODE) {
                  claimStoredHelmTask(stored.key, stored.taskId);
                }
                return;
              }
              const task = res.data;
              const finished = task?.status === "succeeded" || task?.status === "failed";
              if (!finished || !claimStoredHelmTask(stored.key, stored.taskId)) {
                return;
              }
              if (task.status === "succeeded") {
                toast.success(t("helm:{{name}} is installed", {name: stored.releaseName}), {action});
              } else {
                toast.error(t("helm:{{name}} failed to install", {name: stored.releaseName}), {
                  description: task.errorMsg?.slice(0, 300),
                  duration: 15000,
                  action,
                });
              }
            })
            .catch(() => {})
        )
      ).finally(() => {
        polling = false;
      });
    }

    check();
    const timer = setInterval(check, POLL_INTERVAL);
    return () => clearInterval(timer);
  }, [enabled, history, resolvePath, t]);

  return null;
}
