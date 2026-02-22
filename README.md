name: docker dynamic limits

limits:
* CPU total usage (after limit is reached the container should freeze until the limit is increased)
* external paid services spending budget (LLMs, etc)
* disk space usage
* RAM usage
* network total usage (bytes)
* disk IO total bytes
* disk IO total operations

for each container, for each limit I need a way to:
* check current usage
* set limit
* increase limit
* decrease limit

i need a way to clone a container

each container needs a way (from inside) to check its own limits set currently and current usage of that limits


