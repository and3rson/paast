build:
	docker build -t andunai/paast .

run: build
	docker run --rm -it -p 8080:8080 -v ${PWD}/data:/var/lib/paast andunai/paast

push: build
	docker push andunai/paast
