package libpod

import (
	"context"
	"strings"

	"github.com/containers/libpod/libpod/define"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type containerNode struct {
	id         string
	container  *Container
	dependsOn  []*containerNode
	dependedOn []*containerNode
}

type containerGraph struct {
	nodes              map[string]*containerNode
	noDepNodes         []*containerNode
	notDependedOnNodes map[string]*containerNode
}

func buildContainerGraph(ctrs []*Container) (*containerGraph, error) {
	graph := new(containerGraph)
	graph.nodes = make(map[string]*containerNode)
	graph.notDependedOnNodes = make(map[string]*containerNode)

	// Start by building all nodes, with no edges
	for _, ctr := range ctrs {
		ctrNode := new(containerNode)
		ctrNode.id = ctr.ID()
		ctrNode.container = ctr

		graph.nodes[ctr.ID()] = ctrNode
		graph.notDependedOnNodes[ctr.ID()] = ctrNode
	}

	// Now add edges based on dependencies
	for _, node := range graph.nodes {
		deps := node.container.Dependencies()
		for _, dep := range deps {
			// Get the dep's node
			depNode, ok := graph.nodes[dep]
			if !ok {
				return nil, errors.Wrapf(define.ErrNoSuchCtr, "container %s depends on container %s not found in input list", node.id, dep)
			}

			// Add the dependent node to the node's dependencies
			// And add the node to the dependent node's dependedOn
			node.dependsOn = append(node.dependsOn, depNode)
			depNode.dependedOn = append(depNode.dependedOn, node)

			// The dependency now has something depending on it
			delete(graph.notDependedOnNodes, dep)
		}

		// Maintain a list of nodes with no dependencies
		// (no edges coming from them)
		if len(deps) == 0 {
			graph.noDepNodes = append(graph.noDepNodes, node)
		}
	}

	// Need to do cycle detection
	// We cannot start or stop if there are cyclic dependencies
	cycle, err := detectCycles(graph)
	if err != nil {
		return nil, err
	} else if cycle {
		return nil, errors.Wrapf(define.ErrInternal, "cycle found in container dependency graph")
	}

	return graph, nil
}

// Detect cycles in a container graph using Tarjan's strongly connected
// components algorithm
// Return true if a cycle is found, false otherwise
func detectCycles(graph *containerGraph) (bool, error) {
	type nodeInfo struct {
		index   int
		lowLink int
		onStack bool
	}

	index := 0

	nodes := make(map[string]*nodeInfo)
	stack := make([]*containerNode, 0, len(graph.nodes))

	var strongConnect func(*containerNode) (bool, error)
	strongConnect = func(node *containerNode) (bool, error) {
		logrus.Debugf("Strongconnecting node %s", node.id)

		info := new(nodeInfo)
		info.index = index
		info.lowLink = index
		index = index + 1

		nodes[node.id] = info

		stack = append(stack, node)

		info.onStack = true

		logrus.Debugf("Pushed %s onto stack", node.id)

		// Work through all nodes we point to
		for _, successor := range node.dependsOn {
			if _, ok := nodes[successor.id]; !ok {
				logrus.Debugf("Recursing to successor node %s", successor.id)

				cycle, err := strongConnect(successor)
				if err != nil {
					return false, err
				} else if cycle {
					return true, nil
				}

				successorInfo := nodes[successor.id]
				if successorInfo.lowLink < info.lowLink {
					info.lowLink = successorInfo.lowLink
				}
			} else {
				successorInfo := nodes[successor.id]
				if successorInfo.index < info.lowLink && successorInfo.onStack {
					info.lowLink = successorInfo.index
				}
			}
		}

		if info.lowLink == info.index {
			l := len(stack)
			if l == 0 {
				return false, errors.Wrapf(define.ErrInternal, "empty stack in detectCycles")
			}

			// Pop off the stack
			topOfStack := stack[l-1]
			stack = stack[:l-1]

			// Popped item is no longer on the stack, mark as such
			topInfo, ok := nodes[topOfStack.id]
			if !ok {
				return false, errors.Wrapf(define.ErrInternal, "error finding node info for %s", topOfStack.id)
			}
			topInfo.onStack = false

			logrus.Debugf("Finishing node %s. Popped %s off stack", node.id, topOfStack.id)

			// If the top of the stack is not us, we have found a
			// cycle
			if topOfStack.id != node.id {
				return true, nil
			}
		}

		return false, nil
	}

	for id, node := range graph.nodes {
		if _, ok := nodes[id]; !ok {
			cycle, err := strongConnect(node)
			if err != nil {
				return false, err
			} else if cycle {
				return true, nil
			}
		}
	}

	return false, nil
}

// Visit a node on a container graph and start the container, or set an error if
// a dependency failed to start. if restart is true, startNode will restart the node instead of starting it.
func startNode(ctx context.Context, node *containerNode, setError bool, ctrErrors map[string]error, ctrsVisited map[string]bool, restart bool) {
	// First, check if we have already visited the node
	if ctrsVisited[node.id] {
		return
	}

	// If setError is true, a dependency of us failed
	// Mark us as failed and recurse
	if setError {
		// Mark us as visited, and set an error
		ctrsVisited[node.id] = true
		ctrErrors[node.id] = errors.Wrapf(define.ErrCtrStateInvalid, "a dependency of container %s failed to start", node.id)

		// Hit anyone who depends on us, and set errors on them too
		for _, successor := range node.dependedOn {
			startNode(ctx, successor, true, ctrErrors, ctrsVisited, restart)
		}

		return
	}

	// Have all our dependencies started?
	// If not, don't visit the node yet
	depsVisited := true
	for _, dep := range node.dependsOn {
		depsVisited = depsVisited && ctrsVisited[dep.id]
	}
	if !depsVisited {
		// Don't visit us yet, all dependencies are not up
		// We'll hit the dependencies eventually, and when we do it will
		// recurse here
		return
	}

	// Going to try to start the container, mark us as visited
	ctrsVisited[node.id] = true

	ctrErrored := false

	// Check if dependencies are running
	// Graph traversal means we should have started them
	// But they could have died before we got here
	// Does not require that the container be locked, we only need to lock
	// the dependencies
	depsStopped, err := node.container.checkDependenciesRunning()
	if err != nil {
		ctrErrors[node.id] = err
		ctrErrored = true
	} else if len(depsStopped) > 0 {
		// Our dependencies are not running
		depsList := strings.Join(depsStopped, ",")
		ctrErrors[node.id] = errors.Wrapf(define.ErrCtrStateInvalid, "the following dependencies of container %s are not running: %s", node.id, depsList)
		ctrErrored = true
	}

	// Lock before we start
	node.container.lock.Lock()

	// Sync the container to pick up current state
	if !ctrErrored {
		if err := node.container.syncContainer(); err != nil {
			ctrErrored = true
			ctrErrors[node.id] = err
		}
	}

	// Start the container (only if it is not running)
	if !ctrErrored {
		if !restart && node.container.state.State != define.ContainerStateRunning {
			if err := node.container.initAndStart(ctx); err != nil {
				ctrErrored = true
				ctrErrors[node.id] = err
			}
		}
		if restart && node.container.state.State != define.ContainerStatePaused && node.container.state.State != define.ContainerStateUnknown {
			if err := node.container.restartWithTimeout(ctx, node.container.config.StopTimeout); err != nil {
				ctrErrored = true
				ctrErrors[node.id] = err
			}
		}
	}

	node.container.lock.Unlock()

	// Recurse to anyone who depends on us and start them
	for _, successor := range node.dependedOn {
		startNode(ctx, successor, ctrErrored, ctrErrors, ctrsVisited, restart)
	}
}