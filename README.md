Dockerhub deployment webhook for Kubernetes
===========================================

This consists in a really tiny HTTP server meant to receive webhook
notifications from DockerHub once images are built on a given tag.

This little program will figure out if deployments (in the namespaces you
specify) use images that match this `repo/image:tag` combination and, if so,
will update the target deployment in a rolling update fashion using an dirty old
trick.

Important Note
--------------
You shouldn't push your releases on the same tag, and therefore shouldn't need
this script: in order to benefit from Kubernetes roll back features (`kubectl
rollout undo ...`) it's much better to tag images according to the hashtag of
the commit id they are built upon (or the release tag, or any identifier for
every release of your image, would it be a simple date).
Doing so simply makes it impossible to roll back to previous versions of your
container without rebuilding & repushing a former version of the code they're
built upon.

However, some people seem to use such a moving "release tag" on Docker cloud
so...  this might come in handy if you're moving to Kubernetes.

Maintainer
----------
 * Étienne Lafarge <etienne.lafarge@gmail.com>
