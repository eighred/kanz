def spec_containers($pod):
  (($pod.spec.initContainers // []) + ($pod.spec.containers // []));

def init_statuses($pod):
  ($pod.status.initContainerStatuses // []);

def running_statuses($pod):
  ($pod.status.containerStatuses // []);

def observed_statuses($pod):
  (init_statuses($pod) + running_statuses($pod));

def reviewed_image:
  test("^012619468098\\.dkr\\.ecr\\.ap-northeast-1\\.amazonaws\\.com/[a-z0-9-]+@sha256:[0-9a-f]{64}$");

def digest:
  capture("@(?<digest>sha256:[0-9a-f]{64})$").digest;

.items as $pods
| ($pods | length) == 9
  and all($pods[];
    . as $pod
    | $pod.status.phase == "Running"
      and (observed_statuses($pod) | length) == (spec_containers($pod) | length)
      and all(running_statuses($pod)[]; .ready == true)
      and all(init_statuses($pod)[]; .state.terminated.exitCode == 0)
      and all(spec_containers($pod)[]; .image | reviewed_image)
      and all(observed_statuses($pod)[];
        . as $status
        | (spec_containers($pod) | map(select(.name == $status.name))) as $matches
        | ($matches | length) == 1
          and (($matches[0].image | digest) == ($status.imageID | digest))
      )
  )
